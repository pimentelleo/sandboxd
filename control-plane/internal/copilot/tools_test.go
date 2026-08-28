package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestToolsDenyTraversalAndScopeDockerRequests(t *testing.T) {
	executor := &fakeExecutor{result: ScopedExecResult{Stdout: "ok"}}
	tools := taskTools("sandbox_3", executor)
	if _, err := tools[1].Handler(map[string]any{"path": "../secret"}); err == nil {
		t.Fatal("read traversal was allowed")
	}
	if executor.calls != 0 {
		t.Fatal("traversal reached Docker")
	}
	if _, err := tools[3].Handler(map[string]any{"path": "nested/file.txt", "content": "hello"}); err != nil {
		t.Fatal(err)
	}
	request := executor.request
	if request.Container != "s-sandbox_3" || request.User != "sandbox" || request.Workdir != workspaceDir ||
		request.Timeout != toolTimeout || request.OutputLimit != outputLimit || len(request.Stdin) != 0 {
		t.Fatalf("unscoped request: %#v", request)
	}
	command := strings.Join(request.Command, " ")
	if !strings.Contains(command, "readlink -m") || !strings.Contains(command, workspaceDir) {
		t.Fatalf("write lacks symlink containment check: %q", command)
	}
}

func TestToolSchemasAreStrictAndOutputIsBounded(t *testing.T) {
	executor := &fakeExecutor{result: ScopedExecResult{Stdout: strings.Repeat("x", outputLimit+8)}}
	tools := taskTools("sandbox_4", executor)
	for _, tool := range tools {
		if tool.Schema["additionalProperties"] != false {
			t.Fatalf("%s schema permits unexpected properties", tool.Name)
		}
	}
	result, err := tools[4].Handler(map[string]any{"command": "echo hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != outputLimit {
		t.Fatalf("output length = %d", len(result))
	}
	if _, err := tools[4].Handler(map[string]any{"command": "x", "other": "y"}); err == nil {
		t.Fatal("unexpected command argument was accepted")
	}
}

func TestDelegationToolsFollowPlanMutationGate(t *testing.T) {
	delegate := &fakeBackgroundDelegate{}
	gate := newMutationGate(false)
	tools := delegationTools(delegate, BackgroundTaskRequest{
		ConversationID: "conversation-1", TurnID: "turn-1", SandboxID: "sandbox-1",
	}, gate)
	var spawn, cancel RuntimeTool
	for _, tool := range tools {
		switch tool.Name {
		case "delegate_task":
			spawn = tool
		case "cancel_delegated_task":
			cancel = tool
		}
	}
	if _, err := spawn.Handler(map[string]any{"task": "add tests"}); !errors.Is(err, errMutationNotAllowed) {
		t.Fatalf("spawn before approval error = %v; want mutation gate error", err)
	}
	if _, err := cancel.Handler(map[string]any{"task_id": "child-1"}); !errors.Is(err, errMutationNotAllowed) {
		t.Fatalf("cancel before approval error = %v; want mutation gate error", err)
	}
	if delegate.spawned != 0 || delegate.cancelled != 0 {
		t.Fatalf("delegation reached coordinator before plan approval: %#v", delegate)
	}

	gate.Allow()
	if _, err := spawn.Handler(map[string]any{"task": "add tests", "label": "tests"}); err != nil {
		t.Fatalf("spawn after approval: %v", err)
	}
	if _, err := cancel.Handler(map[string]any{"task_id": "child-1"}); err != nil {
		t.Fatalf("cancel after approval: %v", err)
	}
	if delegate.spawned != 1 || delegate.cancelled != 1 {
		t.Fatalf("delegation calls after approval = %#v", delegate)
	}
}

func TestWorkspaceToolContainerTargetRejectsUnknownNames(t *testing.T) {
	sandboxID, childID, isChild, ok := WorkspaceToolContainerTarget("s-sandbox_1")
	if !ok || isChild || sandboxID != "sandbox_1" || childID != "" {
		t.Fatalf("sandbox target = %q, %q, %t, %t", sandboxID, childID, isChild, ok)
	}
	sandboxID, childID, isChild, ok = WorkspaceToolContainerTarget(BackgroundWorkerContainerName("child_1"))
	if !ok || !isChild || sandboxID != "" || childID != "child_1" {
		t.Fatalf("child target = %q, %q, %t, %t", sandboxID, childID, isChild, ok)
	}
	if _, _, _, ok := WorkspaceToolContainerTarget("sandboxd-child-../escape"); ok {
		t.Fatal("invalid worker target was accepted")
	}
}

func TestDelegationToolResponsesRemainBounded(t *testing.T) {
	task := BackgroundTask{
		ID: "child-1", ParentTurnID: "turn-1", Label: strings.Repeat("l", 512),
		Task: strings.Repeat("p", 64<<10), Status: "succeeded", PatchState: "available",
		Result: strings.Repeat("r", 60<<10), ChangedFiles: []string{
			strings.Repeat("a", 1024), strings.Repeat("b", 1024),
		},
	}
	encoded, err := marshalBackgroundTask(task)
	if err != nil {
		t.Fatalf("marshal maximum task result: %v", err)
	}
	if len(encoded) > backgroundTaskToolResultLimit {
		t.Fatalf("task response length = %d; want <= %d", len(encoded), backgroundTaskToolResultLimit)
	}
	var returned BackgroundTask
	if err := json.Unmarshal([]byte(encoded), &returned); err != nil {
		t.Fatalf("decode task response: %v", err)
	}
	if returned.Result == "" || !strings.Contains(returned.Result, backgroundTaskToolTruncationSuffix) {
		t.Fatalf("bounded task result = %q; want retained truncated result", returned.Result)
	}

	tasks := make([]BackgroundTask, 100)
	for i := range tasks {
		tasks[i] = task
	}
	list, err := marshalBackgroundTaskList(tasks)
	if err != nil {
		t.Fatalf("marshal task list: %v", err)
	}
	if len(list) > backgroundTaskToolResultLimit {
		t.Fatalf("task list length = %d; want <= %d", len(list), backgroundTaskToolResultLimit)
	}
	var listed backgroundTaskList
	if err := json.Unmarshal([]byte(list), &listed); err != nil {
		t.Fatalf("decode task list: %v", err)
	}
	if len(listed.Tasks) == 0 {
		t.Fatal("bounded task list omitted every task")
	}

	change, err := marshalBackgroundTaskChange(BackgroundTaskChange{
		TaskID: "child-1", Path: "src/" + strings.Repeat("c", 1000),
		Content: strings.Repeat("d", 48<<10),
	})
	if err != nil {
		t.Fatalf("marshal maximum task change: %v", err)
	}
	if len(change) > backgroundTaskToolResultLimit {
		t.Fatalf("task change length = %d; want <= %d", len(change), backgroundTaskToolResultLimit)
	}
}

type fakeExecutor struct {
	calls   int
	request ScopedExecRequest
	result  ScopedExecResult
}

func (e *fakeExecutor) ExecScoped(_ context.Context, request ScopedExecRequest) (ScopedExecResult, error) {
	e.calls++
	e.request = request
	return e.result, nil
}

type fakeBackgroundDelegate struct {
	spawned   int
	cancelled int
}

func (d *fakeBackgroundDelegate) SpawnBackgroundTask(_ context.Context, _ BackgroundTaskRequest) (BackgroundTask, error) {
	d.spawned++
	return BackgroundTask{ID: "child-1", Status: "queued"}, nil
}

func (d *fakeBackgroundDelegate) ListBackgroundTasks(_ context.Context, _ string) ([]BackgroundTask, error) {
	return nil, nil
}

func (d *fakeBackgroundDelegate) GetBackgroundTask(_ context.Context, _, _ string) (BackgroundTask, error) {
	return BackgroundTask{}, nil
}

func (d *fakeBackgroundDelegate) ReadBackgroundTaskChange(_ context.Context, _, _, _ string) (BackgroundTaskChange, error) {
	return BackgroundTaskChange{}, nil
}

func (d *fakeBackgroundDelegate) CancelBackgroundTask(_ context.Context, _, _ string) (BackgroundTask, error) {
	d.cancelled++
	return BackgroundTask{ID: "child-1", Status: "cancelling"}, nil
}
