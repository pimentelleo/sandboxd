package copilot

import (
	"context"
	"errors"
	"strings"
	"sync"
)

const (
	autopilotAssumption = "Proceed with the least-surprising assumption, then state that assumption clearly in the final response."

	conversationInteractionInstructions = `

## Asking for clarification

Use the native ask_user tool whenever information materially affects the app's
scope, architecture, integrations, data model, authentication, deployment, or
visual direction. This is mandatory when the user explicitly asks you to ask
questions. Ask one concise, highest-impact question at a time, offer choices
when they are genuinely constrained, and allow a freeform answer when needed.
Wait for its response before making dependent changes.

Never present a blocking question only as normal assistant text. Use normal text
for explanations and progress updates, not as a substitute for ask_user. Do not
ask questions merely to defer low-impact work; make a reasonable assumption
instead and disclose it in the final response.`

	backgroundDelegationInstructions = `

## Delegating background work

Use delegate_task only for bounded, independent work that can safely happen in
parallel with your primary task. The delegated agent receives an isolated copy
of the workspace and can never change this workspace directly. Continue your
own work after delegating. Use list_delegated_tasks or get_delegated_task to
inspect completion, then use read_delegated_change to review individual changes
before deciding whether to apply them yourself with the normal workspace tools.
Never assume a delegated change has already been applied.`
)

// ConversationTurnRequest is a hosted, durable conversation turn. It is never
// accepted directly from a sandbox; the control-plane coordinator supplies the
// sandbox ID, callbacks, and immutable selected mode.
type ConversationTurnRequest struct {
	ConversationID string
	SandboxID      string
	TurnID         string
	// WorkspaceContainer is a control-plane selected target for restricted
	// workspace tools. Empty uses the sandbox associated with SandboxID.
	WorkspaceContainer string
	Prompt             string
	Mode               string
	Model              string
	ReasoningEffort    string
	ContextTier        string
	SystemPrompt       string
	OnEvent            func(Envelope)
	OnUserInput        func(RuntimeInputRequest) (RuntimeInputResponse, error)
	OnPlan             func(RuntimePlanRequest) (RuntimePlanResponse, error)
	Delegation         BackgroundDelegate
}

// RunConversationTurn runs exactly one native SDK turn and leaves its durable
// session mapping in place for a later turn. Waiting for an input/plan callback
// consumes no sandbox process: only the control-plane SDK goroutine remains.
func (m *Manager) RunConversationTurn(ctx context.Context, request ConversationTurnRequest) error {
	if !validIdentifier(request.ConversationID) || !validIdentifier(request.SandboxID) ||
		len(request.Prompt) == 0 || len(request.Prompt) > maxPrompt ||
		len(request.Model) > 256 || len(request.ReasoningEffort) > 64 ||
		len(request.ContextTier) > 64 || len(request.SystemPrompt) > maxSystem ||
		!validConversationMode(request.Mode) {
		return errors.New("invalid Copilot conversation turn")
	}
	if request.Delegation != nil && !validIdentifier(request.TurnID) {
		return errors.New("invalid Copilot conversation delegation")
	}
	workspaceContainer := request.WorkspaceContainer
	if workspaceContainer == "" {
		workspaceContainer = sandboxContainerName(request.SandboxID)
	}
	if !validWorkspaceContainer(workspaceContainer) {
		return errors.New("invalid Copilot workspace target")
	}

	taskCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := m.reserveConversation(request.ConversationID, cancel); err != nil {
		return err
	}
	defer m.releaseConversation(request.ConversationID)

	token, account, generation, err := m.conversationCredential(request.ConversationID)
	if err != nil {
		return err
	}
	events := make(chan RuntimeEvent, 64)
	idleEvents := make(chan struct{}, 1)
	sessionError := make(chan struct{})
	var sessionErrorOnce sync.Once
	eventPumpDone := make(chan struct{})
	stream := streamState{redact: redactText(request.ConversationID, request.SandboxID, workspaceContainer, token)}
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
				if event.Type == "error" {
					sessionErrorOnce.Do(func() { close(sessionError) })
					continue
				}
				if stream.handle(event, request.OnEvent) {
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

	gate := newMutationGate(request.Mode != ConversationModePlan)
	runtimeConfig := RuntimeConfig{
		Model: request.Model, ReasoningEffort: request.ReasoningEffort, ContextTier: request.ContextTier,
		Token: token, SystemPrompt: conversationSystemPrompt(request.SystemPrompt, request.Delegation != nil),
		Workdir: m.runtimeWorkdir(), Tools: conversationTools(workspaceContainer, m.cfg.Executor, gate,
			request.Delegation, BackgroundTaskRequest{
				ConversationID: request.ConversationID, TurnID: request.TurnID, SandboxID: request.SandboxID,
			}),
		OnEvent: func(event RuntimeEvent) {
			select {
			case events <- event:
			case <-taskCtx.Done():
			}
		},
		OnUserInputRequest: func(input RuntimeInputRequest) (RuntimeInputResponse, error) {
			if request.Mode == ConversationModeAutopilot {
				return RuntimeInputResponse{Answer: autopilotAssumption, WasFreeform: true}, nil
			}
			if request.OnUserInput == nil {
				return RuntimeInputResponse{}, errors.New("Copilot input request cannot be presented")
			}
			return request.OnUserInput(input)
		},
		OnPlanRequest: func(plan RuntimePlanRequest) (RuntimePlanResponse, error) {
			var (
				response RuntimePlanResponse
				err      error
			)
			if request.Mode == ConversationModeAutopilot {
				response = RuntimePlanResponse{Approved: true, SelectedAction: ConversationModeAutopilot}
			} else if request.OnPlan == nil {
				return RuntimePlanResponse{}, errors.New("Copilot plan request cannot be presented")
			} else {
				response, err = request.OnPlan(plan)
				if err != nil {
					return RuntimePlanResponse{}, err
				}
			}
			if response.Approved && allowsMutation(response.SelectedAction) {
				gate.Allow()
			}
			return response, nil
		},
	}
	session, err := m.openConversationSession(taskCtx, request.ConversationID, account, generation, runtimeConfig)
	if err != nil {
		return errors.New("unable to start Copilot conversation")
	}
	m.setConversationSession(request.ConversationID, session)
	defer session.Disconnect()

	conversationSession, ok := session.(ConversationRuntimeSession)
	if !ok {
		return errors.New("Copilot runtime does not support conversation messages")
	}
	sendResult := make(chan error, 1)
	go func() {
		sendResult <- conversationSession.SendMessage(taskCtx, RuntimeMessage{
			Prompt: request.Prompt, Mode: request.Mode,
		})
	}()

	var (
		sendComplete bool
		idle         bool
	)
	for {
		select {
		case <-sessionError:
			return ErrSessionError
		case err := <-sendResult:
			if err != nil {
				if taskCtx.Err() != nil {
					return taskCtx.Err()
				}
				return errors.New("unable to run Copilot conversation turn")
			}
			sendComplete = true
			if idle {
				if providerErrorReported(sessionError) {
					return ErrSessionError
				}
				return nil
			}
		case <-idleEvents:
			idle = true
			if sendComplete {
				if providerErrorReported(sessionError) {
					return ErrSessionError
				}
				return nil
			}
		case <-taskCtx.Done():
			_ = session.Abort(context.Background())
			return taskCtx.Err()
		}
	}
}

// CancelConversation aborts only the currently active SDK turn. It is
// idempotent; the coordinator records its terminal cancelled result.
func (m *Manager) CancelConversation(conversationID string) {
	m.mu.Lock()
	active, ok := m.activeConversations[conversationID]
	m.mu.Unlock()
	if !ok {
		return
	}
	active.cancel()
	if active.session != nil {
		_ = active.session.Abort(context.Background())
	}
}

// CleanupConversation clears an SDK session when its transcript is archived,
// the sandbox is purged, or credentials change. SDK deletion is best-effort
// after its private mapping has been durably removed.
func (m *Manager) CleanupConversation(conversationID string) error {
	if !validIdentifier(conversationID) {
		return errors.New("invalid conversation ID")
	}
	owner := conversationSessionOwner(conversationID)
	m.mu.Lock()
	m.generations[owner]++
	active, hasActive := m.activeConversations[conversationID]
	delete(m.activeConversations, conversationID)
	sessionID, hadSession := m.sessions[owner]
	delete(m.sessions, owner)
	err := m.persistLocked()
	if err != nil && hadSession {
		m.sessions[owner] = sessionID
	}
	m.mu.Unlock()
	if hasActive {
		active.cancel()
		if active.session != nil {
			_ = active.session.Abort(context.Background())
		}
	}
	if err == nil && sessionID != "" {
		_ = m.runtime.Delete(context.Background(), sessionID)
	}
	return err
}

func (m *Manager) reserveConversation(conversationID string, cancel context.CancelFunc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.activeConversations[conversationID]; exists {
		return errors.New("Copilot conversation is already active")
	}
	m.activeConversations[conversationID] = activeTask{cancel: cancel}
	return nil
}

func (m *Manager) releaseConversation(conversationID string) {
	m.mu.Lock()
	delete(m.activeConversations, conversationID)
	m.mu.Unlock()
}

func (m *Manager) setConversationSession(conversationID string, session RuntimeSession) {
	m.mu.Lock()
	active, ok := m.activeConversations[conversationID]
	if ok {
		active.session = session
		m.activeConversations[conversationID] = active
	}
	m.mu.Unlock()
}

func (m *Manager) conversationCredential(conversationID string) (token, account string, generation uint64, err error) {
	owner := conversationSessionOwner(conversationID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.credential == nil || m.credential.PersonalAccessToken == "" || m.credential.Account == "" {
		return "", "", 0, ErrNotConnected
	}
	return m.credential.PersonalAccessToken, m.credential.Account, m.generations[owner], nil
}

func (m *Manager) openConversationSession(ctx context.Context, conversationID, account string, generation uint64, config RuntimeConfig) (RuntimeSession, error) {
	return m.openSession(ctx, conversationSessionOwner(conversationID), generation, account, nil, config)
}

func conversationSessionOwner(conversationID string) string {
	// A slash cannot occur in a valid sandbox identifier, so a sandbox's
	// legacy bridge mapping can never collide with a conversation mapping.
	return "conversation/" + conversationID
}

func validConversationMode(mode string) bool {
	switch mode {
	case ConversationModeInteractive, ConversationModePlan, ConversationModeAutopilot:
		return true
	default:
		return false
	}
}

func allowsMutation(action string) bool {
	return action == ConversationModeInteractive || action == ConversationModeAutopilot
}

func conversationSystemPrompt(base string, delegationEnabled ...bool) string {
	prompt := base + conversationInteractionInstructions
	if len(delegationEnabled) != 0 && delegationEnabled[0] {
		prompt += backgroundDelegationInstructions
	}
	return prompt
}

const backgroundWorkerContainerPrefix = "sandboxd-child-"

// BackgroundWorkerContainerName returns the opaque, non-preview container name
// for one isolated child. It remains in this package so the executor can accept
// only targets the hosted Copilot tool layer knows how to name.
func BackgroundWorkerContainerName(id string) string {
	return backgroundWorkerContainerPrefix + id
}

func sandboxContainerName(id string) string {
	return "s-" + id
}

// IsWorkspaceToolContainer identifies the only container name families the
// hosted-Copilot executor may service. Per-turn fixed-target wrappers still
// prevent one conversation from selecting another sandbox or child.
func IsWorkspaceToolContainer(name string) bool {
	return validWorkspaceContainer(name)
}

// WorkspaceToolContainerTarget parses a valid hosted-Copilot tool target. The
// control plane uses it to confirm the name still belongs to a live sandbox or
// delegated worker before invoking Docker.
func WorkspaceToolContainerTarget(name string) (sandboxID, childID string, isChild, ok bool) {
	switch {
	case strings.HasPrefix(name, "s-"):
		sandboxID = strings.TrimPrefix(name, "s-")
		return sandboxID, "", false, validIdentifier(sandboxID)
	case strings.HasPrefix(name, backgroundWorkerContainerPrefix):
		childID = strings.TrimPrefix(name, backgroundWorkerContainerPrefix)
		return "", childID, true, validIdentifier(childID)
	default:
		return "", "", false, false
	}
}

func validWorkspaceContainer(name string) bool {
	_, _, _, ok := WorkspaceToolContainerTarget(name)
	return ok
}
