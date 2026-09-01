// Package secrets encrypts app config values at rest with AES-256-GCM
// (standard-library crypto only). Plaintext and key material are never logged.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Profile controls how encryption-key material is sourced.
type Profile string

const (
	// ProfileLocal permits a generated persistent key when no configured key exists.
	ProfileLocal Profile = "local"
	// ProfileProduction requires operator-provided key material and never generates it.
	ProfileProduction Profile = "production"
)

var (
	// ErrProductionKeyRequired indicates production was started without key material.
	ErrProductionKeyRequired = errors.New("secrets: production requires a configured key value or existing key file")
)

// ParseProfile validates the secret-source profile. Empty retains the historical
// local behavior for callers that have not yet been configured explicitly.
func ParseProfile(value string) (Profile, error) {
	switch Profile(strings.ToLower(strings.TrimSpace(value))) {
	case "", ProfileLocal:
		return ProfileLocal, nil
	case ProfileProduction:
		return ProfileProduction, nil
	default:
		return "", fmt.Errorf("secrets: unknown profile %q", value)
	}
}

// Config configures the encryption-key source. Key is a base64-encoded 32-byte
// value. In production KeyFile must already exist; it is intended to be a
// trusted CSI-mounted file. AllowedKeyFileRoots constrains file-backed keys.
type Config struct {
	Profile             Profile
	Key                 string
	KeyFile             string
	AllowedKeyFileRoots []string
}

// Cipher seals/opens secret values.
type Cipher struct{ aead cipher.AEAD }

// Load builds a Cipher. If envKey is set it must be base64 for 32 bytes;
// otherwise a key is read from keyfilePath, or generated and written
// there with 0600 permissions. It retains the historical local profile.
func Load(envKey, keyfilePath string) (*Cipher, error) {
	return LoadWithConfig(Config{Profile: ProfileLocal, Key: envKey, KeyFile: keyfilePath})
}

// LoadProduction builds a Cipher without ever generating key material.
func LoadProduction(key, keyFile string) (*Cipher, error) {
	return LoadWithConfig(Config{Profile: ProfileProduction, Key: key, KeyFile: keyFile})
}

// LoadWithConfig builds a Cipher from an explicitly selected key source.
func LoadWithConfig(config Config) (*Cipher, error) {
	profile, err := ParseProfile(string(config.Profile))
	if err != nil {
		return nil, err
	}
	key, err := resolveKey(config, profile)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

func resolveKey(config Config, profile Profile) ([]byte, error) {
	keyValue := strings.TrimSpace(config.Key)
	keyFile := strings.TrimSpace(config.KeyFile)
	if profile == ProfileProduction && keyValue != "" && keyFile != "" {
		return nil, errors.New("secrets: production key value and key file are mutually exclusive")
	}
	if keyValue != "" {
		key, err := base64.StdEncoding.DecodeString(keyValue)
		if err != nil {
			return nil, fmt.Errorf("secrets: configured key is not valid base64: %w", err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("secrets: configured key must decode to 32 bytes, got %d", len(key))
		}
		return key, nil
	}

	if keyFile != "" {
		validated, err := permittedKeyFile(keyFile, config.AllowedKeyFileRoots)
		if err != nil {
			return nil, err
		}
		if b, err := os.ReadFile(validated); err == nil {
			key, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
			if derr != nil || len(key) != 32 {
				return nil, errors.New("secrets: configured key file is corrupt (want base64 of 32 bytes)")
			}
			if profile == ProfileLocal {
				if err := requirePrivateFile(validated); err != nil {
					return nil, err
				}
			}
			return key, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("secrets: read configured key file: %w", err)
		}
	}
	if profile == ProfileProduction {
		return nil, ErrProductionKeyRequired
	}
	if keyFile == "" {
		return nil, errors.New("secrets: local key file path is required when no key value is configured")
	}

	// Generate and persist a fresh key.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("secrets: create local key file: %w", err)
	}
	if _, err := f.Write([]byte(base64.StdEncoding.EncodeToString(key))); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("secrets: write local key file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("secrets: close local key file: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(keyFile, 0o600); err != nil {
			return nil, fmt.Errorf("secrets: secure local key file permissions: %w", err)
		}
	}
	if err := requirePrivateFile(keyFile); err != nil {
		return nil, err
	}
	return key, nil
}

func permittedKeyFile(path string, allowedRoots []string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("secrets: resolve key file: %w", err)
	}
	if len(allowedRoots) == 0 {
		return absolute, nil
	}
	for _, root := range allowedRoots {
		root, err = filepath.Abs(root)
		if err != nil {
			return "", fmt.Errorf("secrets: resolve allowed key-file root: %w", err)
		}
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return "", errors.New("secrets: permitted key-file root is unavailable")
		}
		resolvedPath, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			// Let the caller give the profile-specific missing-key error instead
			// of treating an absent, as-yet-local key file as a path escape.
			resolvedPath = absolute
		}
		rel, err := filepath.Rel(resolvedRoot, resolvedPath)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return absolute, nil
		}
	}
	return "", errors.New("secrets: configured key file is outside permitted roots")
}

func requirePrivateFile(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("secrets: stat key file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("secrets: key file must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("secrets: key file permissions must not grant group or other access")
	}
	return nil
}

// Seal encrypts plaintext, returning the ciphertext and the random nonce
// used (store both; the nonce is not secret).
func (c *Cipher) Seal(plaintext []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, c.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return c.aead.Seal(nil, nonce, plaintext, nil), nonce, nil
}

// Open decrypts ciphertext sealed with nonce. Fails if either was tampered.
func (c *Cipher) Open(ciphertext, nonce []byte) ([]byte, error) {
	if len(nonce) != c.aead.NonceSize() {
		return nil, errors.New("secrets: wrong nonce length")
	}
	return c.aead.Open(nil, nonce, ciphertext, nil)
}
