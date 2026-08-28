package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
