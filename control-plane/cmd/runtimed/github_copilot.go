package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtime"
)

const (
	copilotBridgeTaskPath   = "/internal/copilot/tasks"
	copilotBridgeCancelPath = "/internal/copilot/tasks/cancel"
	copilotBridgeTimeout    = 5 * time.Second
)

var errCopilotBridgeUnavailable = errors.New("github copilot agent is unavailable")

// githubCopilotAgent dispatches a task to the control-plane bridge. The bridge
// owns the Copilot SDK session and capability validation; runtimed only maps its
// safe NDJSON event envelopes into canonical task events.
type githubCopilotAgent struct{ log *slog.Logger }

func (c *githubCopilotAgent) name() string { return "github-copilot" }

type copilotBridgeEvent struct {
	Type       string `json:"type"`
	Role       string `json:"role"`
	Text       string `json:"text"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Path       string `json:"path"`
	Message    string `json:"message"`
	Input      int    `json:"input"`
	Output     int    `json:"output"`
	Reasoning  int    `json:"reasoning"`
	CacheRead  int    `json:"cache_read"`
	CacheWrite int    `json:"cache_write"`
	Total      int    `json:"total"`
}

type copilotParseResult struct {
	FinalMessage string
	Usage        runtime.TokenUsage
	SawEvent     bool
	APIErr       string
	StreamErr    error
}

// parseGitHubCopilotStream consumes only the bridge's safe event schema. Bad
// and unknown lines are intentionally ignored so future bridge additions do not
// destabilize task execution.
func parseGitHubCopilotStream(r io.Reader, emit eventSink) copilotParseResult {
	var pr copilotParseResult
	var text strings.Builder
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev copilotBridgeEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "message":
			if (ev.Role != "agent" && ev.Role != "agent_error") || ev.Text == "" {
				continue
			}
			pr.SawEvent = true
			emit(runtime.EventMessage, map[string]any{"role": ev.Role, "text": ev.Text})
			if ev.Role == "agent" {
				text.WriteString(ev.Text)
			}
		case "tool":
			if ev.Name == "" || ev.Status == "" {
				continue
			}
			pr.SawEvent = true
			data := map[string]any{"name": ev.Name, "status": ev.Status}
			if ev.Path != "" {
				data["path"] = ev.Path
			}
			emit(runtime.EventTool, data)
		case "usage":
			pr.SawEvent = true
			pr.Usage.Input += ev.Input
			pr.Usage.Output += ev.Output
			pr.Usage.Reasoning += ev.Reasoning
			pr.Usage.CacheRead += ev.CacheRead
			pr.Usage.CacheWrite += ev.CacheWrite
			if ev.Total != 0 {
				pr.Usage.Total += ev.Total
			} else {
				pr.Usage.Total += ev.Input + ev.Output + ev.Reasoning + ev.CacheRead + ev.CacheWrite
			}
		case "error":
			pr.SawEvent = true
			msg := ev.Message
			if msg == "" {
				msg = "github copilot bridge reported an error"
			}
			if pr.APIErr == "" {
				pr.APIErr = msg
			}
			emit(runtime.EventMessage, map[string]any{"role": "agent_error", "text": msg})
		}
	}
	pr.FinalMessage = text.String()
	pr.StreamErr = sc.Err()
	return pr
}

type copilotBridgeRequest struct {
	Capability   string `json:"capability"`
	Prompt       string `json:"prompt"`
	Model        string `json:"model,omitempty"`
	Continue     *bool  `json:"continue,omitempty"`
	SystemPrompt string `json:"system_prompt,omitempty"`
}

type copilotBridgeCancelRequest struct {
	Capability string `json:"capability"`
}

func copilotBridgeEndpoint(raw, path string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawPath = ""
	return u.String(), true
}

func copilotBridgeClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			// The bridge is an internal control-plane endpoint. Never honor a
			// sandbox-provided HTTP proxy, which could receive the capability.
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: copilotBridgeTimeout, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   copilotBridgeTimeout,
			ResponseHeaderTimeout: 15 * time.Second,
			ExpectContinueTimeout: time.Second,
			IdleConnTimeout:       30 * time.Second,
		},
	}
}

func (c *githubCopilotAgent) run(ctx context.Context, spec agentSpec, emit eventSink) (string, runtime.TokenUsage, error) {
	var usage runtime.TokenUsage
	endpoint, ok := copilotBridgeEndpoint(os.Getenv("RUNTIMED_COPILOT_BRIDGE_URL"), copilotBridgeTaskPath)
	if !ok || spec.copilotCapability == "" {
		return "", usage, errCopilotBridgeUnavailable
	}

	body, err := json.Marshal(copilotBridgeRequest{
		Capability: spec.copilotCapability, Prompt: spec.prompt, Model: spec.model,
		Continue: spec.copilotContinue, SystemPrompt: spec.systemPrompt,
	})
	if err != nil {
		return "", usage, errCopilotBridgeUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", usage, errCopilotBridgeUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")

	finished := make(chan struct{})
	var cancelOnce sync.Once
	cancelBridge := func() {
		cancelOnce.Do(func() { c.cancelBridge(endpoint, spec.copilotCapability) })
	}
	defer func() {
		close(finished)
		// When both channels become ready together, the watcher may select
		// finished. Check here as well so cancellation is never skipped.
		if ctx.Err() != nil {
			cancelBridge()
		}
	}()
	go func() {
		select {
		case <-finished:
		case <-ctx.Done():
			cancelBridge()
		}
	}()

	resp, err := copilotBridgeClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", usage, nil
		}
		return "", usage, errors.New("github copilot bridge request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", usage, errors.New("github copilot bridge request failed")
	}

	pr := parseGitHubCopilotStream(resp.Body, emit)
	if ctx.Err() != nil {
		return pr.FinalMessage, pr.Usage, nil
	}
	if pr.APIErr != "" {
		return pr.FinalMessage, pr.Usage, fmt.Errorf("agent error: %s", pr.APIErr)
	}
	if pr.StreamErr != nil {
		return pr.FinalMessage, pr.Usage, errors.New("github copilot bridge stream failed")
	}
	if !pr.SawEvent {
		return pr.FinalMessage, pr.Usage, errors.New("github copilot bridge produced no events")
	}
	return pr.FinalMessage, pr.Usage, nil
}

// cancelBridge deliberately creates a fresh bounded context because the task
// context is already cancelled when this best-effort request is needed.
func (c *githubCopilotAgent) cancelBridge(taskEndpoint, capability string) {
	base := strings.TrimSuffix(taskEndpoint, copilotBridgeTaskPath)
	endpoint, ok := copilotBridgeEndpoint(base, copilotBridgeCancelPath)
	if !ok {
		return
	}
	body, err := json.Marshal(copilotBridgeCancelRequest{Capability: capability})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), copilotBridgeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := copilotBridgeClient().Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
