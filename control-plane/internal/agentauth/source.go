package agentauth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrReadOnlySource is returned when a CSI-mounted credential source is
	// configured for an operation that would mutate credentials.
	ErrReadOnlySource = errors.New("agentauth: credential source is read-only")
)

// CredentialSource provides opaque credential material. It deliberately exposes
// no directory or filesystem access to callers.
type CredentialSource interface {
	ReadCredential(provider, relativePath string) ([]byte, error)
	CredentialMethod(provider string) (string, error)
	ReadOnly() bool
}

// TrustedCredentialSource is sealed to this package so auth proxies cannot be
// accidentally configured with arbitrary untrusted credential readers.
type TrustedCredentialSource interface {
	CredentialSource
	trustedCredentialSource()
}

// emptySource is the production default until an operator configures a
// dedicated control-plane credential source. It never opens a directory or
// accepts writes, so provider credentials cannot accidentally be introduced
// through a host-backed fallback.
type emptySource struct{}

func (emptySource) trustedCredentialSource() {}
func (emptySource) ReadOnly() bool           { return true }
func (emptySource) CredentialMethod(string) (string, error) {
	return "", nil
}
func (emptySource) ReadCredential(string, string) ([]byte, error) {
	return nil, errors.New("agentauth: no credential is configured")
}

// NewEmptyStore returns a read-only credential store with no filesystem
// backing. It is suitable for a Kubernetes control plane that only permits
// keyless OpenCode requests through the private auth proxy.
func NewEmptyStore() *Store {
	return &Store{source: emptySource{}}
}

// MountedSourceConfig describes an operator-mounted, read-only credential tree.
// MountPath and every AllowedRoot must be absolute. The mount must be within an
// allowed root, avoiding accidental reads from the host filesystem.
type MountedSourceConfig struct {
	MountPath    string
	AllowedRoots []string
}

// MountedSource reads global provider credentials from a trusted CSI mount.
// Files use the same fixed provider layout as the local store, but arbitrary
// paths, providers, and path traversal are rejected.
type MountedSource struct {
	root string
}

func (s *MountedSource) trustedCredentialSource() {}

// NewMountedSource validates a CSI mount before it can serve credentials.
func NewMountedSource(config MountedSourceConfig) (*MountedSource, error) {
	if strings.TrimSpace(config.MountPath) == "" {
		return nil, errors.New("agentauth: credential mount path is required")
	}
	if len(config.AllowedRoots) == 0 {
		return nil, errors.New("agentauth: credential mount requires permitted roots")
	}
	root, err := filepath.Abs(config.MountPath)
	if err != nil {
		return nil, fmt.Errorf("agentauth: resolve credential mount: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, errors.New("agentauth: configured credential mount is unavailable")
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, errors.New("agentauth: configured credential mount is unavailable")
	}
	if !info.IsDir() {
		return nil, errors.New("agentauth: configured credential mount is not a directory")
	}
	permitted := false
	for _, allowed := range config.AllowedRoots {
		if !filepath.IsAbs(allowed) {
			return nil, errors.New("agentauth: permitted credential root must be absolute")
		}
		allowed, err = filepath.EvalSymlinks(allowed)
		if err != nil {
			return nil, errors.New("agentauth: permitted credential root is unavailable")
		}
		rel, err := filepath.Rel(allowed, root)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			permitted = true
			break
		}
	}
	if !permitted {
		return nil, errors.New("agentauth: configured credential mount is outside permitted roots")
	}
	return &MountedSource{root: root}, nil
}

// NewMountedStore creates a read-only store backed solely by a validated CSI
// mount. It does not create a writable fallback directory.
func NewMountedStore(config MountedSourceConfig) (*Store, error) {
	source, err := NewMountedSource(config)
	if err != nil {
		return nil, err
	}
	return NewStoreWithSource(source)
}

func (s *MountedSource) ReadOnly() bool { return true }

func (s *MountedSource) CredentialMethod(provider string) (string, error) {
	if b, err := s.ReadCredential(provider, APIKeyFile); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return "api_key", nil
	}
	if rel, ok := CredentialFile(provider); ok {
		if b, err := s.ReadCredential(provider, rel); err == nil && len(b) > 0 {
			return "oauth", nil
		}
	}
	return "", nil
}

func (s *MountedSource) ReadCredential(provider, relativePath string) ([]byte, error) {
	if !allowedCredentialPath(provider, relativePath) {
		return nil, errors.New("agentauth: credential path is not permitted")
	}
	path := filepath.Join(s.root, provider, relativePath)
	// Even a CSI implementation using symlinks must resolve within the mounted
	// root; this prevents an operator typo from exposing host files.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, errors.New("agentauth: configured credential is unavailable")
	}
	rel, err := filepath.Rel(s.root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, errors.New("agentauth: configured credential resolves outside mount")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("agentauth: configured credential is unavailable")
	}
	if info.Size() <= 0 || info.Size() > 1<<20 {
		return nil, errors.New("agentauth: configured credential has an invalid size")
	}
	b, err := os.ReadFile(resolved)
	if err != nil {
		return nil, errors.New("agentauth: configured credential is unavailable")
	}
	return b, nil
}

func allowedCredentialPath(provider, relativePath string) bool {
	if _, ok := Get(provider); !ok {
		return false
	}
	if relativePath == APIKeyFile {
		return true
	}
	want, ok := CredentialFile(provider)
	return ok && relativePath == want
}
