package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtime"
)

func TestParseGitHubCopilotStream(t *testing.T) {
	stream := strings.Join([]string{
		`not json`,
		`{"type":"message","role":"agent","text":"Done "}`,
		`{"type":"tool","name":"edit","status":"completed","path":"main.go"}`,
		`{"type":"usage","input":10,"output":3,"reasoning":2,"cache_read":4,"cache_write":1,"total":20}`,
		`{"type":"usage","input":1,"output":2,"total":3}`,
		`{"type":"message","role":"agent_error","text":"non-fatal notice"}`,
		`{"type":"unknown","secret":"ignored"}`,
	}, "\n")
	sink, events := collectSink()
	pr := parseGitHubCopilotStream(strings.NewReader(stream), sink)

	if got, want := pr.FinalMessage, "Done "; got != want {
		t.Errorf("final message = %q, want %q", got, want)
	}
	if pr.APIErr != "" || pr.StreamErr != nil {
		t.Errorf("unexpected errors: APIErr=%q StreamErr=%v", pr.APIErr, pr.StreamErr)
	}
	want := runtime.TokenUsage{Input: 11, Output: 5, Reasoning: 2, CacheRead: 4, CacheWrite: 1, Total: 23}
	if pr.Usage != want {
		t.Errorf("usage = %+v, want %+v", pr.Usage, want)
	}
	if len(*events) != 3 {
		t.Fatalf("events = %d, want 3", len(*events))
	}
	if (*events)[0].typ != runtime.EventMessage || (*events)[1].typ != runtime.EventTool ||
		(*events)[2].data.(map[string]any)["role"] != "agent_error" {
		t.Errorf("unexpected canonical events: %+v", *events)
	}
}

func TestParseGitHubCopilotStreamErrorFailsAndSurfaces(t *testing.T) {
	sink, events := collectSink()
	pr := parseGitHubCopilotStream(strings.NewReader(`{"type":"error","message":"bridge-safe error"}`), sink)
	if pr.APIErr != "bridge-safe error" {
		t.Errorf("APIErr = %q", pr.APIErr)
	}
	if len(*events) != 1 {
		t.Fatalf("events = %d, want 1", len(*events))
	}
	data := (*events)[0].data.(map[string]any)
	if (*events)[0].typ != runtime.EventMessage || data["role"] != "agent_error" || data["text"] != "bridge-safe error" {
		t.Errorf("error event = %+v", (*events)[0])
	}
}

func TestGitHubCopilotAgentErrorEnvelopeFailsTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"type":"error","message":"safe bridge failure"}`+"\n")
	}))
	defer server.Close()
	t.Setenv("RUNTIMED_COPILOT_BRIDGE_URL", server.URL)

	sink, events := collectSink()
	_, _, err := (&githubCopilotAgent{}).run(context.Background(),
		agentSpec{prompt: "p", copilotCapability: "capability"}, sink)
	if err == nil || !strings.Contains(err.Error(), "safe bridge failure") {
		t.Fatalf("error = %v", err)
	}
	if len(*events) != 1 || (*events)[0].typ != runtime.EventMessage {
		t.Errorf("expected surfaced agent error, got %+v", *events)
	}
}

func TestGitHubCopilotAgentRequestDoesNotLeakTaskSecrets(t *testing.T) {
	const capability = "capability-only-for-this-task"
	const forbidden = "must-not-leak"
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != copilotBridgeTaskPath {
			t.Errorf("path = %q", r.URL.Path)
		}
		if gotAccept := r.Header.Get("Accept"); gotAccept != "application/x-ndjson" {
			t.Errorf("Accept = %q", gotAccept)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"type":"message","role":"agent","text":"complete"}`+"\n")
	}))
	defer server.Close()
	t.Setenv("RUNTIMED_COPILOT_BRIDGE_URL", server.URL)

	sink, _ := collectSink()
	a := &githubCopilotAgent{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var rawLog bytes.Buffer
	final, _, err := a.run(context.Background(), agentSpec{
		prompt: "make change", model: "gpt-test", systemPrompt: "safe briefing",
		copilotCapability: capability, env: map[string]string{"API_TOKEN": forbidden}, rawLog: &rawLog,
	}, sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if final != "complete" {
		t.Errorf("final = %q", final)
	}
	if got["capability"] != capability || got["prompt"] != "make change" || got["model"] != "gpt-test" {
		t.Errorf("request fields = %#v", got)
	}
	if _, exists := got["continue"]; exists {
		t.Errorf("omitted continue must stay omitted for the bridge default: %#v", got)
	}
	if _, exists := got["copilot_capability"]; exists {
		t.Errorf("internal request field leaked into bridge body: %#v", got)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), forbidden) {
		t.Errorf("agent environment leaked into bridge request: %s", encoded)
	}
	if strings.Contains(rawLog.String(), capability) {
		t.Error("capability leaked to agent.log")
	}
}

func TestGitHubCopilotAgentCancelsBridgeWithFreshContext(t *testing.T) {
	const capability = "task-capability"
	taskStarted := make(chan struct{})
	cancelled := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case copilotBridgeTaskPath:
			close(taskStarted)
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			if flush, ok := w.(http.Flusher); ok {
				flush.Flush()
			}
			<-r.Context().Done()
		case copilotBridgeCancelPath:
			var body copilotBridgeCancelRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			cancelled <- body.Capability
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("RUNTIMED_COPILOT_BRIDGE_URL", server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &githubCopilotAgent{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	done := make(chan error, 1)
	go func() {
		_, _, err := a.run(ctx, agentSpec{prompt: "p", copilotCapability: capability}, func(string, any) {})
		done <- err
	}()
	<-taskStarted
	cancel()
	if err := <-done; err != nil {
		t.Errorf("cancelled run error = %v, want nil for task lifecycle", err)
	}
	select {
	case got := <-cancelled:
		if got != capability {
			t.Errorf("cancel capability = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge cancel request was not received")
	}
}

func TestNewTaskGitHubCopilotPreservesContinueTriState(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name string
		in   *bool
	}{
		{name: "omitted"},
		{name: "false", in: boolp(false)},
		{name: "true", in: boolp(true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task, err := newTask(runtime.StartTaskRequest{
				TaskID: tc.name, Prompt: "p", Agent: "github-copilot", Continue: tc.in,
			}, root)
			if err != nil {
				t.Fatal(err)
			}
			if task.copilotContinue == nil && tc.in != nil {
				t.Fatal("explicit continue value was lost")
			}
			if task.copilotContinue != nil && *task.copilotContinue != *tc.in {
				t.Errorf("continue = %v, want %v", *task.copilotContinue, *tc.in)
			}
			if task.cont {
				t.Error("github-copilot must not use runtimed task history for continuation")
			}
			body, err := json.Marshal(copilotBridgeRequest{Continue: task.copilotContinue})
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatal(err)
			}
			got, present := wire["continue"]
			if tc.in == nil && present {
				t.Errorf("omitted continue encoded as %#v", got)
			}
			if tc.in != nil && (!present || got != *tc.in) {
				t.Errorf("continue encoded as %#v (present=%v), want %v", got, present, *tc.in)
			}
			_ = task.eventsW.Close()
		})
	}
}

func TestTaskDoesNotPersistCopilotCapability(t *testing.T) {
	const capability = "never-in-task-artifacts"
	root := t.TempDir()
	task, err := newTask(runtime.StartTaskRequest{
		TaskID: "copilot", Prompt: "p", Agent: "github-copilot", CopilotCapability: capability,
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	task.emit(runtime.EventStatus, map[string]any{"phase": "starting"})
	task.finish(runtime.TaskResult{ID: task.id, FilesChanged: []string{}})

	for _, name := range []string{"events.jsonl", "result.json"} {
		b, err := os.ReadFile(filepath.Join(task.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), capability) {
			t.Errorf("%s contains capability", name)
		}
	}
}

func TestGitHubCopilotAgentRequiresBridgeAndCapability(t *testing.T) {
	a := &githubCopilotAgent{}
	t.Setenv("RUNTIMED_COPILOT_BRIDGE_URL", "")
	if _, _, err := a.run(context.Background(), agentSpec{copilotCapability: "secret"}, func(string, any) {}); !errors.Is(err, errCopilotBridgeUnavailable) {
		t.Errorf("missing bridge error = %v", err)
	}
	t.Setenv("RUNTIMED_COPILOT_BRIDGE_URL", "http://bridge.invalid")
	if _, _, err := a.run(context.Background(), agentSpec{}, func(string, any) {}); !errors.Is(err, errCopilotBridgeUnavailable) {
		t.Errorf("missing capability error = %v", err)
	}
}

func TestCopilotBridgeClientDoesNotUseEnvironmentProxy(t *testing.T) {
	tr, ok := copilotBridgeClient().Transport.(*http.Transport)
	if !ok {
		t.Fatal("bridge client transport is not an *http.Transport")
	}
	if tr.Proxy != nil {
		t.Fatal("bridge client must bypass environment proxies")
	}
}
