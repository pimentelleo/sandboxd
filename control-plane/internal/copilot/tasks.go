package copilot

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// RunTask validates and consumes a task capability, then forwards only safe
// stream envelopes to emit. It never returns credential, sandbox, or SDK
// diagnostics to the caller.
func (m *Manager) RunTask(ctx context.Context, request TaskRequest, emit func(Envelope)) error {
	if len(request.Capability) < 32 || len(request.Prompt) == 0 || len(request.Prompt) > maxPrompt ||
		len(request.Model) > 256 || len(request.SystemPrompt) > maxSystem {
		return errors.New("invalid Copilot task")
	}
	cap, err := m.consumeCapability(request.Capability)
	if err != nil {
		return err
	}
	if !m.capabilityMatchesAccount(request.Capability, cap) {
		m.finishCapability(request.Capability)
		return ErrCapability
	}
	token, err := m.tokenForTask()
	if err != nil {
		m.finishCapability(request.Capability)
		return ErrNotConnected
	}
	if !m.capabilityMatchesAccount(request.Capability, cap) {
		m.finishCapability(request.Capability)
		return ErrCapability
	}

	taskCtx, cancel := context.WithCancel(ctx)
	events := make(chan RuntimeEvent, 64)
	stream := streamState{redact: redactText(request.Capability, cap.sandboxID, "s-"+cap.sandboxID, token)}
	idleEvents := make(chan struct{}, 1)
	eventPumpDone := make(chan struct{})
	var stopEventsOnce sync.Once
	stopEvents := func() {
		stopEventsOnce.Do(func() {
			cancel()
			<-eventPumpDone
		})
	}
	defer stopEvents()
	go func() {
		defer close(eventPumpDone)
		for {
			select {
			case event := <-events:
				if stream.handle(event, emit) {
					select {
					case idleEvents <- struct{}{}:
					default:
					}
				}
			case <-taskCtx.Done():
				return
			}
		}
	}()
	runtimeConfig := RuntimeConfig{
		Model: request.Model, Token: token, SystemPrompt: request.SystemPrompt,
		Workdir: m.runtimeWorkdir(), Tools: taskTools(cap.sandboxID, m.cfg.Executor),
		OnEvent: func(event RuntimeEvent) {
			select {
			case events <- event:
			case <-taskCtx.Done():
			}
		},
	}
	session, err := m.openSession(taskCtx, cap.sandboxID, cap.generation, cap.account, request.Continue, runtimeConfig)
	if err != nil {
		m.finishCapability(request.Capability)
		return errors.New("unable to start Copilot task")
	}
	m.mu.Lock()
	if current, ok := m.capabilities[request.Capability]; !ok || !current.consumed ||
		current.account != cap.account || m.credential == nil || m.credential.Account != cap.account {
		m.mu.Unlock()
		_ = session.Abort(context.Background())
		_ = session.Disconnect()
		return ErrCapability
	}
	m.active[request.Capability] = activeTask{session: session, cancel: cancel}
	m.mu.Unlock()
	defer func() {
		m.finishCapability(request.Capability)
		stopEvents()
		// Disconnect releases SDK in-memory handlers while preserving the durable
		// session ID that the next task can resume.
		_ = session.Disconnect()
	}()

	// SDK event delivery is usually asynchronous, but a runtime implementation
	// may invoke callbacks synchronously while opening or sending a session.
	// Drain events concurrently so a verbose response cannot fill the bounded
	// queue and deadlock the SDK callback.
	sendResult := make(chan error, 1)
	go func() {
		sendResult <- session.Send(taskCtx, request.Prompt)
	}()
	var (
		sendComplete bool
		idle         bool
	)
	for {
		select {
		case err := <-sendResult:
			if err != nil {
				return errors.New("unable to start Copilot task")
			}
			sendComplete = true
			if idle {
				return nil
			}
		case <-idleEvents:
			idle = true
			if sendComplete {
				return nil
			}
		case <-taskCtx.Done():
			// Cancellation is intentionally an ordinary clean stream ending.
			_ = session.Abort(context.Background())
			return nil
		}
	}
}

func (m *Manager) runtimeWorkdir() string {
	// The SDK must never receive a sandbox workspace directory. Its own durable
	// state directory is configured in New; this is an inert control-plane cwd.
	return filepath.Join(m.cfg.StateDir, "copilot-runtime")
}

func (m *Manager) openSession(ctx context.Context, sandboxID string, generation uint64, account string, continuation *bool, config RuntimeConfig) (RuntimeSession, error) {
	m.mu.Lock()
	if m.generations[sandboxID] != generation || m.credential == nil || m.credential.Account != account {
		m.mu.Unlock()
		return nil, errors.New("sandbox was cleaned up")
	}
	sessionID := m.sessions[sandboxID]
	m.mu.Unlock()
	resume := sessionID != ""
	if continuation != nil {
		resume = *continuation && sessionID != ""
	}
	var (
		session RuntimeSession
		err     error
	)
	if resume {
		session, err = m.runtime.Resume(ctx, sessionID, config)
	} else {
		session, err = m.runtime.Create(ctx, config)
	}
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.generations[sandboxID] != generation || m.credential == nil || m.credential.Account != account {
		m.mu.Unlock()
		_ = session.Disconnect()
		return nil, errors.New("sandbox was cleaned up")
	}
	previous, hadPrevious := m.sessions[sandboxID]
	m.sessions[sandboxID] = session.ID()
	if err := m.persistLocked(); err != nil {
		if hadPrevious {
			m.sessions[sandboxID] = previous
		} else {
			delete(m.sessions, sandboxID)
		}
		m.mu.Unlock()
		_ = session.Disconnect()
		return nil, err
	}
	m.mu.Unlock()
	if !resume && hadPrevious && previous != "" && previous != session.ID() {
		_ = m.runtime.Delete(context.Background(), previous)
	}
	return session, nil
}

func (m *Manager) finishCapability(token string) {
	m.mu.Lock()
	delete(m.active, token)
	delete(m.capabilities, token)
	m.mu.Unlock()
}

type streamState struct {
	mu       sync.Mutex
	hadDelta bool
	toolName map[string]string
	redact   func(string) string
}

func (s *streamState) handle(event RuntimeEvent, emit func(Envelope)) bool {
	if emit == nil {
		emit = func(Envelope) {}
	}
	redact := s.redact
	if redact == nil {
		redact = func(value string) string { return value }
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch event.Type {
	case "message_delta":
		s.hadDelta = true
		if event.Text != "" {
			emit(Envelope{Type: "message", Role: "agent", Text: redact(event.Text)})
		}
	case "message":
		// Streaming SDK sessions produce deltas and then a final full message.
		// Avoid duplicating the final text while still supporting non-streaming
		// runtime adapters used by tests or future SDK versions.
		if !s.hadDelta && event.Text != "" {
			emit(Envelope{Type: "message", Role: "agent", Text: redact(event.Text)})
		}
	case "tool_start":
		if !isBridgeTool(event.ToolName) {
			return false
		}
		if s.toolName == nil {
			s.toolName = make(map[string]string)
		}
		s.toolName[event.ToolCallID] = event.ToolName
		emit(Envelope{Type: "tool", Name: event.ToolName, Status: "running"})
	case "tool_complete":
		name := s.toolName[event.ToolCallID]
		if name != "" {
			status := "failed"
			if event.Success {
				status = "completed"
			}
			emit(Envelope{Type: "tool", Name: name, Status: status})
			delete(s.toolName, event.ToolCallID)
		}
	case "usage":
		emit(Envelope{Type: "usage", Input: event.Input, Output: event.Output, Reasoning: event.Reasoning,
			CacheRead: event.CacheRead, CacheWrite: event.CacheWrite,
			Total: event.Input + event.Output + event.Reasoning})
	case "idle":
		return true
	}
	return false
}

func isBridgeTool(name string) bool {
	switch name {
	case "list_files", "read_file", "search_files", "write_file", "run_command":
		return true
	default:
		return false
	}
}

var githubTokenPattern = regexp.MustCompile(`\b(?:github_pat_[A-Za-z0-9_]{20,}|gh[opsu]_[A-Za-z0-9_]{20,})\b`)

func redactText(values ...string) func(string) string {
	return func(text string) string {
		for _, value := range values {
			if value != "" {
				text = strings.ReplaceAll(text, value, "[redacted]")
			}
		}
		return githubTokenPattern.ReplaceAllString(text, "[redacted]")
	}
}
