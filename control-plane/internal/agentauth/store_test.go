package agentauth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryHasExpectedProviders(t *testing.T) {
	got := map[string]Provider{}
	for _, p := range Providers() {
		got[p.ID] = p
	}
	for _, want := range []string{"opencode", "claude-code", "codex", "minimax"} {
		p, ok := got[want]
		if !ok {
			t.Errorf("registry missing %q", want)
			continue
		}
		if p.Label == "" {
			t.Errorf("provider %q missing label: %+v", want, p)
		}
		// minimax is a credential-only provider (no task-agent CLI), so it has
		// no binary; every runnable agent must carry one.
		if want != "minimax" && p.Binary == "" {
			t.Errorf("provider %q missing binary: %+v", want, p)
		}
	}
	if _, ok := Get("opencode"); !ok {
		t.Error("Get(opencode) should resolve")
	}
	if _, ok := Get("nope"); ok {
		t.Error("Get(nope) should not resolve")
	}
}

func TestMountedStoreIsReadOnlyAndConstrained(t *testing.T) {
	root := t.TempDir()
	providerDir := filepath.Join(root, "minimax")
	if err := os.MkdirAll(providerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(providerDir, APIKeyFile), []byte("mounted-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewMountedStore(MountedSourceConfig{
		MountPath:    root,
		AllowedRoots: []string{root},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !store.ReadOnly() || store.Method("minimax") != "api_key" {
		t.Fatal("mounted source must expose the configured API key as read-only")
	}
	if _, err := store.ReadCredential("minimax", "../outside"); err == nil {
		t.Fatal("path traversal was accepted")
	}
	if err := store.ImportCredential("minimax", APIKeyFile, []byte("new")); !errors.Is(err, ErrReadOnlySource) {
		t.Fatalf("ImportCredential error = %v; want ErrReadOnlySource", err)
	}
	if err := store.Delete("minimax"); !errors.Is(err, ErrReadOnlySource) {
		t.Fatalf("Delete error = %v; want ErrReadOnlySource", err)
	}
}

func TestMountedStoreRequiresPermittedExistingMount(t *testing.T) {
	root := t.TempDir()
	if _, err := NewMountedStore(MountedSourceConfig{MountPath: root}); err == nil {
		t.Fatal("mounted store without permitted roots was accepted")
	}
	if _, err := NewMountedStore(MountedSourceConfig{
		MountPath:    root,
		AllowedRoots: []string{filepath.Join(root, "elsewhere")},
	}); err == nil {
		t.Fatal("mounted store outside permitted root was accepted")
	}
}

// Connected = dir exists AND non-empty; contents are never read.
func TestStoreConnectedIsPresenceOnly(t *testing.T) {
	data := t.TempDir()
	s := NewStore(data)
	if err := s.EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(s.Root()); got != "agent-auth" {
		t.Errorf("root = %s", s.Root())
	}

	// Absent provider dir => not connected (A0 never creates provider dirs).
	if s.Connected("claude-code") {
		t.Error("absent provider dir should be not-connected")
	}
	// Empty dir => not connected.
	if err := os.MkdirAll(s.Dir("claude-code"), 0o700); err != nil {
		t.Fatal(err)
	}
	if s.Connected("claude-code") {
		t.Error("empty provider dir should be not-connected")
	}
	// Non-empty (an opaque blob) => connected. We never open it.
	if err := os.WriteFile(filepath.Join(s.Dir("claude-code"), ".credentials.json"), []byte("opaque"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !s.Connected("claude-code") {
		t.Error("non-empty provider dir should be connected")
	}
}

// The store root is under the data dir, never inside a sandbox workspace.
func TestStoreRootOutsideWorkspaces(t *testing.T) {
	s := NewStore("/var/lib/sandboxd")
	if s.Root() != filepath.Join("/var/lib/sandboxd", "agent-auth") {
		t.Errorf("root = %s", s.Root())
	}
	if filepath.Dir(s.Dir("opencode")) == filepath.Join("/var/lib/sandboxd", "workspaces") {
		t.Error("auth dir must not be under workspaces/")
	}
}
