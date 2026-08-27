package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtime"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

func TestTaskRequestRejectsPrivateCopilotCapability(t *testing.T) {
	var request v1TaskSubmitReq
	decoder := json.NewDecoder(strings.NewReader(`{"prompt":"work","agent":"github-copilot","copilot_capability":"private"}`))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err == nil {
		t.Fatal("public task request accepted an internal Copilot capability")
	}
}

func TestV1GetTaskReturnsTerminalFallbackWhenResultWasNotStored(t *testing.T) {
	st, err := store.Open(context.Background(), "file::memory:?_fk=1", "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreateTask(context.Background(), &store.Task{
		TaskID: "task-terminal-no-result", SandboxID: "sandbox-terminal-no-result",
		Agent: "github-copilot", Prompt: "recover me", Status: string(runtime.TaskFailed),
		ExecutionKind: store.TaskExecutionHostedCopilot,
	}); err != nil {
		t.Fatalf("create terminal task: %v", err)
	}

	server := &Server{Store: st}
	request := httptest.NewRequest(http.MethodGet,
		"/v1/sandboxes/sandbox-terminal-no-result/tasks/task-terminal-no-result", nil)
	request.SetPathValue("id", "sandbox-terminal-no-result")
	request.SetPathValue("taskId", "task-terminal-no-result")
	response := httptest.NewRecorder()
	server.v1GetTask(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
	}
	var result runtime.TaskResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Status != runtime.TaskFailed || result.FailureReason != "result_unavailable" {
		t.Fatalf("terminal fallback = %#v", result)
	}
}
