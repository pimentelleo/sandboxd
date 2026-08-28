package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/copilot"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/loopback"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtime"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

func TestRecoverConversationsFinalizesPreparedHostedTurn(t *testing.T) {
	ctx := context.Background()
	const (
		sandboxID      = "sandbox-recovery"
		conversationID = "conversation-recovery"
		turnID         = "turn-recovery"
		taskID         = "task-recovery"
	)
	root := t.TempDir()
	socketDir := filepath.Join(root, sandboxID, ".runtimed")
	if err := os.MkdirAll(socketDir, 0o755); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("unix", filepath.Join(socketDir, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	finalized := make(chan runtime.FinalizeHostedTaskRequest, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hosted-tasks/{id}/finalize", func(w http.ResponseWriter, r *http.Request) {
		var request runtime.FinalizeHostedTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode finalize request: %v", err)
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		request.TaskID = r.PathValue("id")
		finalized <- request
		_ = json.NewEncoder(w).Encode(runtime.TaskResult{
			ID: request.TaskID, Prompt: "interrupted request", Status: runtime.TaskFailed,
			FailureReason: request.FailureReason, ErrorMessage: request.ErrorMessage,
			FilesChanged: []string{}, BuildStatus: runtime.BuildSkipped,
		})
	})
	runtimeServer := &http.Server{Handler: mux}
	go runtimeServer.Serve(listener)
	t.Cleanup(func() { runtimeServer.Close() })

	st, err := store.Open(ctx, "file::memory:?_fk=1", "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Create(ctx, &store.Sandbox{
		ID: sandboxID, Status: "running", Image: "sandboxd-base:test",
		WorkspaceImg: filepath.Join(root, sandboxID), WorkspaceMnt: filepath.Join(root, sandboxID),
		MemoryHigh: "4G",
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	conversation := &store.Conversation{
		ID: conversationID, SandboxID: sandboxID, Agent: store.ConversationAgentGitHubCopilot,
		DefaultMode: store.ConversationModeInteractive,
	}
	if err := st.CreateConversation(ctx, conversation); err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if _, _, err := st.EnqueueConversationTurn(ctx, conversationID, turnID, taskID,
		"interrupted request", store.ConversationModeInteractive, store.ConversationTurnSettings{}); err != nil {
		t.Fatalf("enqueue turn: %v", err)
	}
	if _, err := st.ClaimNextConversationTurn(ctx, conversationID); err != nil {
		t.Fatalf("claim turn: %v", err)
	}

	server := &Server{
		Store:    st,
		Loopback: &loopback.Manager{Root: root},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	NewConversationCoordinator(server).Recover(ctx)

	select {
	case request := <-finalized:
		if request.TaskID != taskID || request.Status != runtime.TaskFailed ||
			request.FailureReason != "provider_interrupted" {
			t.Fatalf("finalize request = %#v", request)
		}
	default:
		t.Fatal("recovery did not finalize the prepared hosted task")
	}
	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("get recovered task: %v", err)
	}
	if task.Status != string(runtime.TaskFailed) || !task.ResultJSON.Valid {
		t.Fatalf("recovered task = %#v", task)
	}
	active, err := st.ListActiveConversationTurns(ctx)
	if err != nil {
		t.Fatalf("list active turns: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("active turns after recovery = %#v", active)
	}
}

func TestConversationSubmitSnapshotsValidatedModelSettings(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, "file::memory:?_fk=1", "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	const (
		sandboxID      = "sandbox-model-settings"
		conversationID = "conversation-model-settings"
	)
	if err := st.Create(ctx, &store.Sandbox{
		ID: sandboxID, Status: "running", Image: "sandboxd-base:test",
		WorkspaceImg: t.TempDir(), WorkspaceMnt: t.TempDir(), MemoryHigh: "4G",
	}); err != nil {
		t.Fatal(err)
	}
	conversation := &store.Conversation{
		ID: conversationID, SandboxID: sandboxID, Agent: store.ConversationAgentGitHubCopilot,
		DefaultMode: store.ConversationModeInteractive,
	}
	if err := st.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}

	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	defer authServer.Close()
	modelRuntime := &apiCatalogRuntime{models: []copilot.ModelInfo{{
		ID: "gpt-5.3-codex", Name: "GPT-5.3 Codex",
		SupportedReasoningEfforts: []string{"low", "high"},
	}}}
	manager := newCopilotManagerWithRuntimeForAPITest(t, authServer.Client(), authServer.URL, modelRuntime)
	if _, err := manager.ConnectPAT(ctx, apiTestFineGrainedPAT); err != nil {
		t.Fatal(err)
	}

	coordinator := NewConversationCoordinator(&Server{Store: st, Copilot: manager})
	coordinator.mu.Lock()
	coordinator.workers[conversationID] = true // Keep the queued turn inert for this persistence assertion.
	coordinator.mu.Unlock()
	turn, _, err := coordinator.Submit(ctx, sandboxID, "Build the app", store.ConversationModeInteractive,
		"gpt-5.3-codex", "high", copilot.ContextTierLongContext)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Model != "gpt-5.3-codex" || turn.ReasoningEffort != "high" ||
		turn.ContextTier != copilot.ContextTierLongContext {
		t.Fatalf("submitted model settings = %#v", turn)
	}
	if _, _, err := coordinator.Submit(ctx, sandboxID, "Use an invalid effort", store.ConversationModeInteractive,
		"gpt-5.3-codex", "medium", copilot.ContextTierDefault); !errors.Is(err, copilot.ErrInvalidModelSelection) {
		t.Fatalf("incompatible effort error = %v; want ErrInvalidModelSelection", err)
	}
	snapshot, err := st.SnapshotActiveConversation(ctx, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].Model != turn.Model ||
		snapshot.Turns[0].ReasoningEffort != turn.ReasoningEffort ||
		snapshot.Turns[0].ContextTier != turn.ContextTier {
		t.Fatalf("persisted turn settings = %#v", snapshot.Turns)
	}
}

func TestConversationChildProjectionRedactsPrivateWorkerState(t *testing.T) {
	child := &store.ConversationChild{
		ID: "child-1", ParentTurnID: "turn-1", Label: "Add tests", Prompt: "Write tests",
		Model: "gpt-5.6-terra", ReasoningEffort: "max", ContextTier: "long_context",
		Status: store.ConversationChildSucceeded, WorkspacePath: "/private/worker",
		WorkerContainer: "sandboxd-child-child-1", PatchJSON: `{"credentials":"nope"}`,
		Result: "done", PatchState: store.ConversationChildPatchUnavailable,
	}
	item := v1ConversationChildFromStore(child)
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{child.WorkspacePath, child.WorkerContainer, "credentials"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("public child projection exposed %q: %s", forbidden, raw)
		}
	}
	if item.Task != child.Prompt || item.Status != child.Status {
		t.Fatalf("child projection = %#v", item)
	}
}

func TestConversationChildWorkspaceCopyExcludesRuntimeAndCredentialFiles(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	for _, name := range []string{".env", ".env.staging", ".npmrc", ".netrc", ".pypirc", "package.json"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(name), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(source, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyTreeExcluding(source, destination, conversationChildIgnoredNames); err != nil {
		t.Fatalf("copy child workspace: %v", err)
	}
	for _, excluded := range []string{".env", ".env.staging", ".npmrc", ".netrc", ".pypirc", ".git"} {
		if _, err := os.Stat(filepath.Join(destination, excluded)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("excluded child file %q was copied: %v", excluded, err)
		}
	}
	if _, err := os.Stat(filepath.Join(destination, "package.json")); err != nil {
		t.Fatalf("project source was not copied: %v", err)
	}
}

func TestConversationChildPatchExcludesWorkerCreatedCredentialFiles(t *testing.T) {
	baseline := t.TempDir()
	worker := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseline, "package.json"), []byte(`{"name":"before"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worker, "package.json"), []byte(`{"name":"after"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worker, ".env.local"), []byte("SECRET=not-a-patch"), 0o600); err != nil {
		t.Fatal(err)
	}

	state, patch, err := buildConversationChildPatch(baseline, worker)
	if err != nil {
		t.Fatalf("build child patch: %v", err)
	}
	if state != store.ConversationChildPatchAvailable || len(patch.Changes) != 1 {
		t.Fatalf("child patch = %q, %#v", state, patch)
	}
	change := patch.Changes[0]
	if change.Path != "package.json" || change.Content != `{"name":"after"}` {
		t.Fatalf("child change = %#v", change)
	}
}

func TestConversationChildPatchRejectsTooManyFiles(t *testing.T) {
	baseline := t.TempDir()
	worker := t.TempDir()
	for i := 0; i <= store.MaxConversationChildPatchFiles; i++ {
		if err := os.WriteFile(filepath.Join(baseline, fmt.Sprintf("file-%03d.txt", i)), []byte("before"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := buildConversationChildPatch(baseline, worker); !errors.Is(err, errConversationChildPatch) {
		t.Fatalf("oversized child patch error = %v; want %v", err, errConversationChildPatch)
	}
}

func TestConversationChildRoutesExposeOnlyReviewableChanges(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, "file::memory:?_fk=1", "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	const (
		sandboxID      = "sandbox-child-routes"
		conversationID = "conversation-child-routes"
		turnID         = "turn-child-routes"
		childID        = "child-routes"
	)
	if err := st.Create(ctx, &store.Sandbox{
		ID: sandboxID, Status: "running", Image: "sandboxd-base:test",
		WorkspaceImg: t.TempDir(), WorkspaceMnt: t.TempDir(), MemoryHigh: "4G",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateConversation(ctx, &store.Conversation{
		ID: conversationID, SandboxID: sandboxID, Agent: store.ConversationAgentGitHubCopilot,
		DefaultMode: store.ConversationModeInteractive,
	}); err != nil {
		t.Fatal(err)
	}
	turn, _, err := st.EnqueueConversationTurn(ctx, conversationID, turnID, "task-child-routes",
		"delegate a route test", store.ConversationModeInteractive, store.ConversationTurnSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimNextConversationTurn(ctx, conversationID); err != nil {
		t.Fatal(err)
	}
	child := &store.ConversationChild{
		ID: childID, ConversationID: conversationID, ParentTurnID: turn.ID,
		Label: "Route test", Prompt: "Add the route test.", WorkspacePath: "/private/child-routes",
	}
	if err := st.CreateConversationChild(ctx, child); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimConversationChild(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	child, err = st.StartConversationChild(ctx, child.ID, "sandboxd-child-child-routes")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinishConversationChild(ctx, child.ID, store.ConversationChildSucceeded, "done", "",
		store.ConversationChildPatchAvailable, &store.ConversationChildPatch{Changes: []store.ConversationChildChange{{
			Path: "src/routes/app.go", BaseSHA256: "base", Content: "package routes",
		}}}); err != nil {
		t.Fatal(err)
	}

	handler := (&Server{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet,
		"/v1/sandboxes/"+sandboxID+"/conversation/children", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list delegated tasks status = %d: %s", list.Code, list.Body.String())
	}
	for _, forbidden := range []string{child.WorkspacePath, child.WorkerContainer, "package routes"} {
		if strings.Contains(list.Body.String(), forbidden) {
			t.Fatalf("list delegated tasks exposed %q: %s", forbidden, list.Body.String())
		}
	}

	change := httptest.NewRecorder()
	handler.ServeHTTP(change, httptest.NewRequest(http.MethodGet,
		"/v1/sandboxes/"+sandboxID+"/conversation/children/"+childID+"/changes/src%2Froutes%2Fapp.go", nil))
	if change.Code != http.StatusOK {
		t.Fatalf("get delegated change status = %d: %s", change.Code, change.Body.String())
	}
	var response v1ConversationChildChange
	if err := json.NewDecoder(change.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Path != "src/routes/app.go" || response.Content != "package routes" {
		t.Fatalf("delegated change response = %#v", response)
	}
}
