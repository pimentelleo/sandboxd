package secrets

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	c, err := Load("", filepath.Join(t.TempDir(), "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte("sk-super-secret-value")
	ct, nonce, err := c.Seal(pt)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, pt) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := c.Open(ct, nonce)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("open = %q, %v; want round-trip", got, err)
	}
}

func TestParseProfile(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Profile
		ok   bool
	}{
		{"", ProfileLocal, true},
		{" LOCAL ", ProfileLocal, true},
		{"production", ProfileProduction, true},
		{"staging", "", false},
	} {
		got, err := ParseProfile(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseProfile(%q) = %q, %v", tc.in, got, err)
		}
	}
}

func TestProductionRequiresConfiguredKey(t *testing.T) {
	_, err := LoadWithConfig(Config{Profile: ProfileProduction})
	if !errors.Is(err, ErrProductionKeyRequired) {
		t.Fatalf("LoadWithConfig production = %v; want ErrProductionKeyRequired", err)
	}
	path := filepath.Join(t.TempDir(), "missing-key")
	_, err = LoadWithConfig(Config{Profile: ProfileProduction, KeyFile: path})
	if !errors.Is(err, ErrProductionKeyRequired) {
		t.Fatalf("production missing key file = %v; want ErrProductionKeyRequired", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("production created missing key file: %v", err)
	}
}

func TestProductionLoadsConfiguredKeyValueAndFile(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	if _, err := LoadWithConfig(Config{Profile: ProfileProduction, Key: key}); err != nil {
		t.Fatalf("configured key value: %v", err)
	}

	root := t.TempDir()
	path := filepath.Join(root, "key")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithConfig(Config{
		Profile:             ProfileProduction,
		KeyFile:             path,
		AllowedKeyFileRoots: []string{root},
	}); err != nil {
		t.Fatalf("configured key file: %v", err)
	}
	if _, err := LoadWithConfig(Config{
		Profile:             ProfileProduction,
		KeyFile:             path,
		AllowedKeyFileRoots: []string{filepath.Join(root, "other")},
	}); err == nil {
		t.Fatal("key file outside permitted root was accepted")
	}
}

func TestSealUsesFreshNonce(t *testing.T) {
	c, _ := Load("", filepath.Join(t.TempDir(), "k"))
	_, n1, _ := c.Seal([]byte("x"))
	_, n2, _ := c.Seal([]byte("x"))
	if bytes.Equal(n1, n2) {
		t.Error("nonce reused across two Seals")
	}
}

func TestOpenRejectsTamper(t *testing.T) {
	c, _ := Load("", filepath.Join(t.TempDir(), "k"))
	ct, nonce, _ := c.Seal([]byte("value"))
	ct[0] ^= 0xff // flip a bit
	if _, err := c.Open(ct, nonce); err == nil {
		t.Error("tampered ciphertext decrypted without error")
	}
}

func TestKeyfileGeneratedWith0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "secrets.key")
	if _, err := Load("", path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("keyfile perms = %v; want 0600", fi.Mode().Perm())
	}
	// A second Load reuses the same key (stable across restarts).
	c1, _ := Load("", path)
	c2, _ := Load("", path)
	ct, nonce, _ := c1.Seal([]byte("persisted"))
	got, err := c2.Open(ct, nonce)
	if err != nil || string(got) != "persisted" {
		t.Errorf("key not stable across loads: %q %v", got, err)
	}
}

func TestEnvKeyValidation(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if _, err := Load(good, ""); err != nil {
		t.Errorf("valid 32-byte env key rejected: %v", err)
	}
	if _, err := Load(base64.StdEncoding.EncodeToString(make([]byte, 16)), ""); err == nil {
		t.Error("16-byte key accepted; want error")
	}
	if _, err := Load("not-base64!!", ""); err == nil {
		t.Error("non-base64 key accepted; want error")
	}
}
