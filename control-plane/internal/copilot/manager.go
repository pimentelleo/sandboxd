package copilot

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdk "github.com/github/copilot-sdk/go"
)

const stateFileName = "copilot-state.json"
const stateVersion = 2

type credential struct {
	PersonalAccessToken string `json:"personal_access_token"`
	Account             string `json:"account"`
}

type encryptedCredential struct {
	Ciphertext string `json:"ciphertext"`
	Nonce      string `json:"nonce"`
}

type diskState struct {
	Version    int                  `json:"version"`
	Credential *encryptedCredential `json:"credential,omitempty"`
	Sessions   map[string]string    `json:"sessions,omitempty"`
}

type capability struct {
	sandboxID  string
	taskID     string
	account    string
	generation uint64
	expiresAt  time.Time
	consumed   bool
}

type activeTask struct {
	session RuntimeSession
	cancel  context.CancelFunc
}

// Manager owns personal access tokens, durable SDK session references,
// capabilities, and the private task bridge. It never exposes credentials.
type Manager struct {
	cfg Config

	mu                   sync.Mutex
	credential           *credential
	credentialGeneration uint64
	sessions             map[string]string
	capabilities         map[string]capability
	active               map[string]activeTask
	activeConversations  map[string]activeTask
	generations          map[string]uint64
	runtime              RuntimeClient
	modelCatalog         []ModelInfo
	modelCatalogFetch    *modelCatalogFetch
}

// New initializes state without starting the SDK runtime or contacting GitHub.
func New(cfg Config) (*Manager, error) {
	if strings.TrimSpace(cfg.StateDir) == "" {
		return nil, errors.New("copilot StateDir is required")
	}
	if cfg.Cipher == nil {
		return nil, errors.New("copilot Cipher is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	} else {
		clone := *cfg.HTTPClient
		cfg.HTTPClient = &clone
	}
	// GitHub validation requests carry credentials. Never follow an endpoint-controlled
	// redirect, including when a caller supplied a custom HTTP client.
	cfg.HTTPClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.UserURL == "" {
		cfg.UserURL = "https://api.github.com/user"
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create copilot state directory: %w", err)
	}
	if err := os.Chmod(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure copilot state directory: %w", err)
	}
	m := &Manager{
		cfg: cfg, sessions: make(map[string]string),
		capabilities: make(map[string]capability), active: make(map[string]activeTask),
		activeConversations: make(map[string]activeTask),
		generations:         make(map[string]uint64),
	}
	if cfg.Runtime != nil {
		m.runtime = cfg.Runtime
	} else {
		runtimeDir := filepath.Join(cfg.StateDir, "copilot-runtime")
		if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
			return nil, fmt.Errorf("create copilot runtime directory: %w", err)
		}
		catalogDir := filepath.Join(cfg.StateDir, "copilot-model-catalog")
		if err := os.MkdirAll(catalogDir, 0o700); err != nil {
			return nil, fmt.Errorf("create Copilot model catalog directory: %w", err)
		}
		m.runtime = &sdkRuntime{
			client: sdk.NewClient(&sdk.ClientOptions{
				Mode: sdk.ModeEmpty, BaseDirectory: runtimeDir, WorkingDirectory: runtimeDir,
				UseLoggedInUser: sdk.Bool(false), LogLevel: "error",
			}),
			modelCatalogBaseDirectory: catalogDir,
		}
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{}
	if m.credential != nil && m.credential.Account != "" {
		s.Connected, s.Account, s.Method = true, m.credential.Account, "github-pat"
	}
	return s
}

// Disconnect atomically clears the credential and all state created with it.
func (m *Manager) Disconnect() error {
	m.mu.Lock()
	credential := m.credential
	sessions := m.sessions
	capabilities := m.capabilities
	m.credential = nil
	m.sessions = make(map[string]string)
	m.capabilities = make(map[string]capability)
	active := m.active
	m.active = make(map[string]activeTask)
	activeConversations := m.activeConversations
	m.activeConversations = make(map[string]activeTask)
	err := m.persistLocked()
	if err != nil {
		m.credential = credential
		m.sessions = sessions
		m.capabilities = capabilities
		m.active = active
		m.activeConversations = activeConversations
	} else {
		m.credentialGeneration++
		m.invalidateModelCatalogLocked()
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	for _, task := range active {
		task.cancel()
		if task.session != nil {
			_ = task.session.Abort(context.Background())
		}
	}
	for _, task := range activeConversations {
		task.cancel()
		if task.session != nil {
			_ = task.session.Abort(context.Background())
		}
	}
	for _, sessionID := range sessions {
		if sessionID != "" {
			_ = m.runtime.Delete(context.Background(), sessionID)
		}
	}
	m.invalidateRuntimeModelCatalog()
	return err
}

func (m *Manager) statePath() string { return filepath.Join(m.cfg.StateDir, stateFileName) }

func (m *Manager) load() error {
	b, err := os.ReadFile(m.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read copilot state: %w", err)
	}
	var state diskState
	if err := json.Unmarshal(b, &state); err != nil {
		return errors.New("invalid copilot state")
	}
	if state.Version == 1 {
		// Version 1 only supported OAuth Device Flow. Deliberately discard its
		// credential and associated session references so an upgrade requires a
		// fresh fine-grained PAT rather than silently retaining a legacy token.
		return m.persistLocked()
	}
	if state.Version != stateVersion {
		return errors.New("unsupported copilot state version")
	}
	if state.Sessions != nil {
		m.sessions = state.Sessions
	}
	if state.Credential != nil {
		ciphertext, err := base64.RawStdEncoding.DecodeString(state.Credential.Ciphertext)
		if err != nil {
			return errors.New("invalid encrypted copilot credential")
		}
		nonce, err := base64.RawStdEncoding.DecodeString(state.Credential.Nonce)
		if err != nil {
			return errors.New("invalid encrypted copilot credential")
		}
		plain, err := m.cfg.Cipher.Open(ciphertext, nonce)
		if err != nil {
			return errors.New("unable to decrypt copilot credential")
		}
		var credential credential
		if err := json.Unmarshal(plain, &credential); err != nil || credential.PersonalAccessToken == "" || credential.Account == "" {
			return errors.New("invalid copilot credential")
		}
		m.credential = &credential
	}
	return nil
}

func (m *Manager) persistLocked() error {
	state := diskState{Version: stateVersion, Sessions: m.sessions}
	if m.credential != nil {
		plain, err := json.Marshal(m.credential)
		if err != nil {
			return err
		}
		ciphertext, nonce, err := m.cfg.Cipher.Seal(plain)
		if err != nil {
			return err
		}
		state.Credential = &encryptedCredential{
			Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
			Nonce:      base64.RawStdEncoding.EncodeToString(nonce),
		}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.cfg.StateDir, ".copilot-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, m.statePath())
}

func randomOpaque(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
