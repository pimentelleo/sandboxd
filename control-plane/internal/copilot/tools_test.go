package copilot

import (
	"context"
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
