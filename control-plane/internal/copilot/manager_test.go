package copilot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/secrets"
)

func newTestManager(t *testing.T, now *time.Time, runtime RuntimeClient, client *http.Client, userURLs ...string) *Manager {
	t.Helper()
	stateDir := t.TempDir()
	cipher, err := secrets.Load("", filepath.Join(stateDir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		StateDir:   stateDir,
		Cipher:     cipher,
		Runtime:    runtime,
		HTTPClient: client,
		Now:        func() time.Time { return *now },
	}
	if len(userURLs) == 1 {
		cfg.UserURL = userURLs[0]
	}
	manager, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

const testFineGrainedPAT = "github_pat_abcdefghijklmnopqrstuvwxyz0123456789"

func TestConnectPATPersistsOnlyEncryptedCredential(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/user" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testFineGrainedPAT {
			t.Errorf("authorization header = %q", got)
		}
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	defer server.Close()

	manager := newTestManager(t, &now, &fakeRuntime{}, server.Client(), server.URL+"/user")
	status, err := manager.ConnectPAT(context.Background(), testFineGrainedPAT)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(filepath.Join(manager.cfg.StateDir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), testFineGrainedPAT) {
		t.Fatal("PAT persisted in plaintext")
	}
	var state diskState
	if err := json.Unmarshal(persisted, &state); err != nil {
		t.Fatal(err)
	}
	if state.Version != stateVersion || state.Credential == nil {
		t.Fatalf("persisted state = %#v", state)
	}
	if !status.Connected || status.Account != "octocat" || status.Method != "github-pat" {
		t.Fatalf("unsafe status result: %#v", status)
	}
}

func TestConnectPATRejectsNonFineGrainedToken(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	manager := newTestManager(t, &now, &fakeRuntime{}, nil)
	if _, err := manager.ConnectPAT(context.Background(), "ghp_classic-token"); !errors.Is(err, ErrInvalidPAT) {
		t.Fatalf("classic PAT error = %v; want ErrInvalidPAT", err)
	}
}

func TestLegacyOAuthStateIsDiscarded(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	manager := newTestManager(t, &now, &fakeRuntime{}, nil)
	plain := []byte(`{"access_token":"legacy-access-token","account":"octocat"}`)
	ciphertext, nonce, err := manager.cfg.Cipher.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	legacy := diskState{
		Version: 1,
		Credential: &encryptedCredential{
			Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
			Nonce:      base64.RawStdEncoding.EncodeToString(nonce),
		},
		Sessions: map[string]string{"sandbox_legacy": "session-legacy"},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.statePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.load(); err != nil {
		t.Fatal(err)
	}
	if manager.Status().Connected || len(manager.sessions) != 0 {
		t.Fatalf("legacy state remained connected: status=%#v sessions=%#v", manager.Status(), manager.sessions)
	}
	persisted, err := os.ReadFile(manager.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), "legacy-access-token") || strings.Contains(string(persisted), "session-legacy") {
		t.Fatal("legacy OAuth state survived migration")
	}
}

func TestCapabilityIsBoundOneTimeAndTaskStreamIsSafe(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	runtime := &fakeRuntime{}
	manager := newTestManager(t, &now, runtime, nil)
	manager.mu.Lock()
	manager.credential = &credential{PersonalAccessToken: "access-secret", Account: "octocat"}
	manager.mu.Unlock()

	capability, err := manager.IssueCapability("sandbox_1", "task_1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var got []Envelope
	err = manager.RunTask(context.Background(), TaskRequest{Capability: capability, Prompt: "do work", Continue: boolPointer(false)}, func(e Envelope) {
		got = append(got, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.created != 1 || runtime.resumed != 0 || runtime.config.Workdir == workspaceDir {
		t.Fatalf("unsafe runtime config: %+v", runtime)
	}
	if runtime.disconnected != 1 {
		t.Fatalf("session disconnects = %d, want 1", runtime.disconnected)
	}
	serialized, _ := json.Marshal(got)
	if strings.Contains(string(serialized), capability) || strings.Contains(string(serialized), "sandbox_1") || strings.Contains(string(serialized), "access-secret") {
		t.Fatalf("secret leaked to stream: %s", serialized)
	}
	if err := manager.RunTask(context.Background(), TaskRequest{Capability: capability, Prompt: "again"}, nil); !errors.Is(err, ErrCapability) {
		t.Fatalf("capability reuse error = %v", err)
	}
}

func TestContinuationAndCleanupDeleteDurableSession(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	runtime := &fakeRuntime{}
	manager := newTestManager(t, &now, runtime, nil)
	manager.mu.Lock()
	manager.credential = &credential{PersonalAccessToken: "token", Account: "octocat"}
	manager.mu.Unlock()
	for _, continuation := range []bool{false, true} {
		capability, err := manager.IssueCapability("sandbox_2", "task", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.RunTask(context.Background(), TaskRequest{Capability: capability, Prompt: "x", Continue: boolPointer(continuation)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.created != 1 || runtime.resumed != 1 {
		t.Fatalf("create/resume = %d/%d", runtime.created, runtime.resumed)
	}
	if err := manager.CleanupSandbox("sandbox_2"); err != nil {
		t.Fatal(err)
	}
	if runtime.deleted != "session-1" {
		t.Fatalf("deleted session = %q", runtime.deleted)
	}
	if _, ok := manager.sessions["sandbox_2"]; ok {
		t.Fatal("durable session mapping remained after cleanup")
	}
}

func TestCapabilitiesExpireAndCancellationAbortsActiveSession(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	manager := newTestManager(t, &now, &fakeRuntime{}, nil)
	expired, err := manager.IssueCapability("sandbox_5", "task_5", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := manager.consumeCapability(expired); !errors.Is(err, ErrCapability) {
		t.Fatalf("expired capability error = %v", err)
	}

	token, err := manager.IssueCapability("sandbox_5", "task_6", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.consumeCapability(token); err != nil {
		t.Fatal(err)
	}
	session := &abortSession{}
	_, cancel := context.WithCancel(context.Background())
	manager.mu.Lock()
	manager.active[token] = activeTask{session: session, cancel: cancel}
	manager.mu.Unlock()
	manager.CancelCapability(token)
	if session.aborts != 1 {
		t.Fatalf("cancellation abort calls = %d", session.aborts)
	}
	manager.CleanupSandbox("sandbox_5") // idempotent after cancellation
}

func TestDisconnectRevokesUnconsumedCapabilities(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	runtime := &fakeRuntime{}
	manager := newTestManager(t, &now, runtime, nil)
	manager.mu.Lock()
	manager.credential = &credential{PersonalAccessToken: "old-token", Account: "octocat"}
	manager.mu.Unlock()
	capability, err := manager.IssueCapability("sandbox_7", "task_7", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Disconnect(); err != nil {
		t.Fatal(err)
	}
	// Simulate a subsequent connection. A capability minted for the disconnected
	// account must not become valid under that account.
	manager.mu.Lock()
	manager.credential = &credential{PersonalAccessToken: "new-token", Account: "hubot"}
	manager.mu.Unlock()
	if err := manager.RunTask(context.Background(), TaskRequest{Capability: capability, Prompt: "do not run"}, nil); !errors.Is(err, ErrCapability) {
		t.Fatalf("old capability after disconnect = %v, want ErrCapability", err)
	}
	if runtime.created != 0 {
		t.Fatalf("runtime started after revoked capability: %d", runtime.created)
	}
}

func TestConnectPATAccountChangeClearsPreviousSessionsCapabilitiesAndTasks(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testFineGrainedPAT {
			t.Errorf("authorization header = %q", got)
		}
		_, _ = w.Write([]byte(`{"login":"hubot"}`))
	}))
	defer server.Close()

	runtime := &fakeRuntime{}
	manager := newTestManager(t, &now, runtime, server.Client(), server.URL+"/user")
	manager.mu.Lock()
	manager.credential = &credential{PersonalAccessToken: "old-access-token", Account: "octocat"}
	manager.sessions["sandbox_8"] = "session-octocat"
	manager.mu.Unlock()
	capability, err := manager.IssueCapability("sandbox_8", "task_8", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	activeSession := &abortSession{}
	_, cancel := context.WithCancel(context.Background())
	manager.mu.Lock()
	manager.active["active-task"] = activeTask{session: activeSession, cancel: cancel}
	manager.mu.Unlock()
	status, err := manager.ConnectPAT(context.Background(), testFineGrainedPAT)
	if err != nil || !status.Connected || status.Account != "hubot" {
		t.Fatalf("account change status = %#v, %v", status, err)
	}
	if runtime.deleted != "session-octocat" {
		t.Fatalf("previous session deletion = %q", runtime.deleted)
	}
	if activeSession.aborts != 1 {
		t.Fatalf("previous task aborts = %d", activeSession.aborts)
	}
	if len(manager.sessions) != 0 || len(manager.capabilities) != 0 || len(manager.active) != 0 {
		t.Fatalf("previous account state remained: sessions=%#v capabilities=%#v active=%#v", manager.sessions, manager.capabilities, manager.active)
	}
	if err := manager.RunTask(context.Background(), TaskRequest{Capability: capability, Prompt: "do not run"}, nil); !errors.Is(err, ErrCapability) {
		t.Fatalf("old-account capability = %v, want ErrCapability", err)
	}
}

func TestStreamRedactsKnownAndGitHubShapedSecrets(t *testing.T) {
	var got []Envelope
	stream := streamState{redact: redactText("capability-secret", "sandbox_6", "access-secret")}
	stream.handle(RuntimeEvent{Type: "message", Text: "capability-secret sandbox_6 access-secret github_pat_abcdefghijklmnopqrstuvwxyz_0123456789"}, func(envelope Envelope) {
		got = append(got, envelope)
	})
	stream.handle(RuntimeEvent{Type: "tool_start", ToolCallID: "a", ToolName: "capability-secret"}, func(envelope Envelope) {
		got = append(got, envelope)
	})
	if len(got) != 1 || strings.Contains(got[0].Text, "secret") || strings.Contains(got[0].Text, "github_pat_") {
		t.Fatalf("unsafe stream envelopes: %#v", got)
	}
}

type fakeRuntime struct {
	created, resumed int
	deleted          string
	disconnected     int
	config           RuntimeConfig
}

func (r *fakeRuntime) Create(_ context.Context, config RuntimeConfig) (RuntimeSession, error) {
	r.created++
	r.config = config
	return &fakeSession{id: "session-1", event: config.OnEvent, onDisconnect: func() { r.disconnected++ }}, nil
}

func (r *fakeRuntime) Resume(_ context.Context, _ string, config RuntimeConfig) (RuntimeSession, error) {
	r.resumed++
	r.config = config
	return &fakeSession{id: "session-1", event: config.OnEvent, onDisconnect: func() { r.disconnected++ }}, nil
}

func (r *fakeRuntime) Delete(_ context.Context, id string) error {
	r.deleted = id
	return nil
}

type fakeSession struct {
	id           string
	event        func(RuntimeEvent)
	onDisconnect func()
}

func (s *fakeSession) ID() string { return s.id }

func (s *fakeSession) Send(_ context.Context, _ string) error {
	s.event(RuntimeEvent{Type: "message_delta", Text: "safe response"})
	s.event(RuntimeEvent{Type: "tool_start", ToolCallID: "tool-1", ToolName: "read_file"})
	s.event(RuntimeEvent{Type: "tool_complete", ToolCallID: "tool-1", Success: true})
	s.event(RuntimeEvent{Type: "usage", Input: 4, Output: 3})
	s.event(RuntimeEvent{Type: "idle"})
	return nil
}

func (s *fakeSession) Abort(context.Context) error { return nil }

func (s *fakeSession) Disconnect() error {
	if s.onDisconnect != nil {
		s.onDisconnect()
	}
	return nil
}

type abortSession struct{ aborts int }

func (s *abortSession) ID() string                         { return "abort-session" }
func (s *abortSession) Send(context.Context, string) error { return nil }
func (s *abortSession) Abort(context.Context) error        { s.aborts++; return nil }
func (s *abortSession) Disconnect() error                  { return nil }

func boolPointer(value bool) *bool { return &value }
