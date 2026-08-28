package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	toolTimeout = 30 * time.Second
	outputLimit = 64 << 10
)

var errMutationNotAllowed = errors.New("workspace mutation is not allowed until the plan is approved")

func taskTools(sandboxID string, executor ScopedExecutor) []RuntimeTool {
	return toolsForSandbox(sandboxID, executor, nil)
}

func conversationTools(container string, executor ScopedExecutor, gate *mutationGate, delegate BackgroundDelegate, parent BackgroundTaskRequest) []RuntimeTool {
	tools := toolsForContainer(container, executor, gate)
	if delegate == nil {
		return tools
	}
	return append(tools, delegationTools(delegate, parent, gate)...)
}

func toolsForSandbox(sandboxID string, executor ScopedExecutor, gate *mutationGate) []RuntimeTool {
	return toolsForContainer(sandboxContainerName(sandboxID), executor, gate)
}

func toolsForContainer(container string, executor ScopedExecutor, gate *mutationGate) []RuntimeTool {
	t := toolRunner{container: container, executor: fixedTargetExecutor{target: container, next: executor}, gate: gate}
	return []RuntimeTool{
		{Name: "list_files", Description: "List files beneath a workspace path.", Schema: objectSchema(map[string]any{"path": stringSchema(1024)}), Handler: t.list},
		{Name: "read_file", Description: "Read a UTF-8 text file from the workspace.", Schema: strictSchema(map[string]any{"path": stringSchema(1024)}, "path"), Handler: t.read},
		{Name: "search_files", Description: "Search workspace text files for a literal string.", Schema: strictSchema(map[string]any{"query": stringSchema(512), "path": stringSchema(1024)}, "query"), Handler: t.search},
		{Name: "write_file", Description: "Write a UTF-8 text file under the workspace.", Schema: strictSchema(map[string]any{"path": stringSchema(1024), "content": stringSchema(128 << 10)}, "path", "content"), Handler: t.write},
		{Name: "run_command", Description: "Run a bounded shell command in the workspace.", Schema: strictSchema(map[string]any{"command": stringSchema(8 << 10)}, "command"), Handler: t.run},
	}
}

func objectSchema(properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
}

func strictSchema(properties map[string]any, required ...string) map[string]any {
	schema := objectSchema(properties)
	schema["required"] = required
	return schema
}

func stringSchema(max int) map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "maxLength": max}
}

type toolRunner struct {
	container string
	executor  ScopedExecutor
	gate      *mutationGate
}

func (t toolRunner) list(args any) (string, error) {
	values, err := objectArguments(args, map[string]int{"path": 1024})
	if err != nil {
		return "", err
	}
	p := values["path"]
	if p == "" {
		p = "."
	}
	p, err = workspacePath(p)
	if err != nil {
		return "", err
	}
	return t.exec([]string{"sh", "-c", resolveScript("test -d \"$p\"; find \"$p\" -xdev -maxdepth 4 -printf '%P\\n' | head -n 1000"), "sh", p})
}

func (t toolRunner) read(args any) (string, error) {
	values, err := objectArguments(args, map[string]int{"path": 1024})
	if err != nil || values["path"] == "" {
		return "", errors.New("invalid read request")
	}
	p, err := workspacePath(values["path"])
	if err != nil {
		return "", err
	}
	return t.exec([]string{"sh", "-c", resolveScript("test -f \"$p\"; head -c 65536 -- \"$p\""), "sh", p})
}

func (t toolRunner) search(args any) (string, error) {
	values, err := objectArguments(args, map[string]int{"query": 512, "path": 1024})
	if err != nil || values["query"] == "" || strings.ContainsRune(values["query"], 0) {
		return "", errors.New("invalid search request")
	}
	p := values["path"]
	if p == "" {
		p = "."
	}
	p, err = workspacePath(p)
	if err != nil {
		return "", err
	}
	return t.exec([]string{"sh", "-c", resolveScript("test -e \"$p\"; grep -rIn --binary-files=without-match --exclude-dir=.git -- \"$2\" \"$p\" | head -n 1000 || test $? -eq 1"), "sh", p, values["query"]})
}

func (t toolRunner) write(args any) (string, error) {
	if !t.mayMutate() {
		return "", errMutationNotAllowed
	}
	values, err := objectArguments(args, map[string]int{"path": 1024, "content": 128 << 10})
	if err != nil || values["path"] == "" || values["content"] == "" || strings.ContainsRune(values["content"], 0) {
		return "", errors.New("invalid write request")
	}
	p, err := workspacePath(values["path"])
	if err != nil {
		return "", err
	}
	return t.exec([]string{"sh", "-c", resolveScript("mkdir -p -- \"$(dirname -- \"$p\")\"; printf %s \"$2\" > \"$p\"; printf 'file written\\n'"), "sh", p, values["content"]})
}

func (t toolRunner) run(args any) (string, error) {
	if !t.mayMutate() {
		return "", errMutationNotAllowed
	}
	values, err := objectArguments(args, map[string]int{"command": 8 << 10})
	if err != nil || values["command"] == "" || strings.ContainsRune(values["command"], 0) {
		return "", errors.New("invalid command")
	}
	// The user command remains positional data; this package never interpolates
	// it into generated shell source.
	return t.exec([]string{"sh", "-c", "exec sh -lc \"$1\"", "sh", values["command"]})
}

func (t toolRunner) mayMutate() bool {
	return t.gate == nil || t.gate.Allowed()
}

// mutationGate is one conversation turn's enforcement point. It protects the
// host-owned restricted tool handlers rather than trusting model instructions.
type mutationGate struct {
	mu      sync.RWMutex
	allowed bool
}

func newMutationGate(allowed bool) *mutationGate {
	return &mutationGate{allowed: allowed}
}

func (g *mutationGate) Allowed() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.allowed
}

func (g *mutationGate) Allow() {
	g.mu.Lock()
	g.allowed = true
	g.mu.Unlock()
}

// resolveScript canonicalizes inside the container immediately before each file
// action, rejecting both lexical traversal and symlink escapes.
func resolveScript(action string) string {
	return "set -eu; p=$(readlink -m -- \"$1\"); case \"$p\" in " +
		workspaceDir + "|" + workspaceDir + "/*) ;; *) exit 64;; esac; " + action
}

func (t toolRunner) exec(command []string) (string, error) {
	if t.executor == nil || !validWorkspaceContainer(t.container) {
		return "", errors.New("executor unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	result, err := t.executor.ExecScoped(ctx, ScopedExecRequest{
		Container: t.container, User: "sandbox", Workdir: workspaceDir,
		Command: command, Timeout: toolTimeout, OutputLimit: outputLimit,
	})
	if err != nil || result.ExitCode != 0 {
		return "", errors.New("sandbox operation failed")
	}
	output := result.Stdout
	if output == "" {
		output = result.Stderr
	}
	if len(output) > outputLimit {
		output = output[:outputLimit]
	}
	return output, nil
}

// fixedTargetExecutor turns a broad control-plane executor into a per-turn
// capability. Tool schemas cannot provide a container name, and even an
// internal caller cannot redirect this runner to another workspace.
type fixedTargetExecutor struct {
	target string
	next   ScopedExecutor
}

func (e fixedTargetExecutor) ExecScoped(ctx context.Context, request ScopedExecRequest) (ScopedExecResult, error) {
	if e.next == nil || request.Container != e.target {
		return ScopedExecResult{}, errors.New("invalid workspace target")
	}
	return e.next.ExecScoped(ctx, request)
}

func workspacePath(value string) (string, error) {
	if value == "" || len(value) > 1024 || strings.ContainsRune(value, 0) || path.IsAbs(value) {
		return "", errors.New("invalid workspace path")
	}
	clean := path.Clean(value)
	if clean == "." {
		return clean, nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("invalid workspace path")
	}
	return clean, nil
}

func objectArguments(args any, limits map[string]int) (map[string]string, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, errors.New("invalid tool input")
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, errors.New("invalid tool input")
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		limit, ok := limits[key]
		if !ok {
			return nil, errors.New("invalid tool input")
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil || len(text) > limit {
			return nil, errors.New("invalid tool input")
		}
		out[key] = text
	}
	return out, nil
}
