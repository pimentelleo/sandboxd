package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/agentprompt"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/copilot"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtime"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

const (
	conversationPromptLimit      = 128 << 10
	conversationInteractionLimit = 64 << 10
)

var (
	errConversationUnavailable = errors.New("GitHub Copilot conversations are unavailable")
	errInteractionUnavailable  = errors.New("the Copilot interaction is no longer active")
	errInvalidConversation     = errors.New("invalid Copilot conversation request")
	errSandboxUnavailable      = errors.New("sandbox unavailable for Copilot conversation")
)

// ConversationCoordinator serializes each sandbox's hosted Copilot turns. It
// owns only live SDK callbacks; SQLite remains the source of truth for the
// transcript, queue, and interactions.
type ConversationCoordinator struct {
	server *Server

	mu      sync.Mutex
	workers map[string]bool
	runs    map[string]*conversationRun
	updates map[string]chan struct{}
}

type conversationRun struct {
	conversationID string
	sandboxID      string
	turnID         string
	taskID         string
	budget         *turnBudget

	mu        sync.Mutex
	waiters   map[string]chan interactionDelivery
	assistant string
	tokens    runtime.TokenUsage
	active    bool
	cancelled bool
}

type interactionDelivery struct {
	answer         string
	approved       bool
	selectedAction string
	feedback       string
	isPlan         bool
}

// turnBudget excludes time spent parked in a native user-input or plan
// callback, while still bounding active provider execution.
type turnBudget struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	timer     *time.Timer
	remaining time.Duration
	startedAt time.Time
	paused    bool
	expired   bool
	stopped   bool
}

func newTurnBudget(limit time.Duration) *turnBudget {
	ctx, cancel := context.WithCancel(context.Background())
	b := &turnBudget{
		ctx:       ctx,
		cancel:    cancel,
		remaining: limit,
		startedAt: time.Now(),
	}
	b.timer = time.AfterFunc(limit, b.expire)
	return b
}

func (b *turnBudget) Context() context.Context { return b.ctx }

func (b *turnBudget) expire() {
	b.mu.Lock()
	if b.stopped || b.paused {
		b.mu.Unlock()
		return
	}
	b.expired = true
	b.mu.Unlock()
	b.cancel()
}

func (b *turnBudget) Pause() {
	b.mu.Lock()
	if b.stopped || b.paused || b.expired {
		b.mu.Unlock()
		return
	}
	elapsed := time.Since(b.startedAt)
	if elapsed >= b.remaining {
		b.expired = true
		b.mu.Unlock()
		b.cancel()
		return
	}
	b.remaining -= elapsed
	b.paused = true
	b.timer.Stop()
	b.mu.Unlock()
}

func (b *turnBudget) Resume() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped || !b.paused || b.expired {
		return
	}
	b.startedAt = time.Now()
	b.paused = false
	b.timer.Reset(b.remaining)
}

func (b *turnBudget) Cancel() {
	b.mu.Lock()
	if !b.stopped {
		b.stopped = true
		b.timer.Stop()
	}
	b.mu.Unlock()
	b.cancel()
}

func (b *turnBudget) Stop() {
	b.mu.Lock()
	if !b.stopped {
		b.stopped = true
		b.timer.Stop()
	}
	b.mu.Unlock()
}

func (b *turnBudget) Expired() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.expired
}

// NewConversationCoordinator constructs the hosted-Copilot coordination layer.
// The Server owns its lifecycle so all public routes share one callback map.
func NewConversationCoordinator(server *Server) *ConversationCoordinator {
	return &ConversationCoordinator{
		server:  server,
		workers: make(map[string]bool),
		runs:    make(map[string]*conversationRun),
		updates: make(map[string]chan struct{}),
	}
}

func (s *Server) conversationCoordinator() *ConversationCoordinator {
	s.conversationMu.Lock()
	defer s.conversationMu.Unlock()
	if s.Conversations == nil && s.Copilot != nil {
		s.Conversations = NewConversationCoordinator(s)
	}
	return s.Conversations
}

// RecoverConversations makes any hosted turn that lost its in-process SDK
// callback during a control-plane restart terminal before serving new work.
func (s *Server) RecoverConversations(ctx context.Context) {
	coordinator := s.conversationCoordinator()
	if coordinator == nil {
		return
	}
	coordinator.Recover(ctx)
}

// Recover makes an interrupted hosted SDK call explicit rather than accepting
// a response against a callback that no longer exists after process restart.
// Queued turns are then started with the retained conversation session.
func (c *ConversationCoordinator) Recover(ctx context.Context) {
	turns, err := c.server.Store.ListActiveConversationTurns(ctx)
	if err != nil {
		c.logError("list active conversations for recovery", err)
		return
	}
	for _, turn := range turns {
		if err := c.recoverInterruptedTurn(ctx, turn,
			"Copilot was interrupted because sandboxd restarted. Send a new message to continue."); err != nil {
			c.logError("recover interrupted conversation", err)
			continue
		}
		c.notify(turn.ConversationID)
		c.Start(turn.ConversationID)
	}
}

// Submit creates the first conversation on demand, persists a queued user turn,
// and starts its worker. Model controls are resolved and validated here so
// settings changes cannot alter a turn while it waits in the FIFO queue.
func (c *ConversationCoordinator) Submit(ctx context.Context, sandboxID, prompt, mode, model, reasoningEffort, contextTier string) (*store.ConversationTurn, int, error) {
	if c == nil || c.server == nil || c.server.Copilot == nil {
		return nil, 0, errConversationUnavailable
	}
	if len(prompt) == 0 || len(prompt) > conversationPromptLimit || !validConversationMode(mode) {
		return nil, 0, errInvalidConversation
	}
	if _, err := c.server.Store.Get(ctx, sandboxID); err != nil {
		return nil, 0, err
	}
	if model == "" && c.server.Live != nil {
		model = c.server.Live.DefaultModel(githubCopilotAgentID)
	}
	selection, err := c.server.Copilot.ValidateModelSelection(ctx, model, reasoningEffort, contextTier)
	if err != nil {
		return nil, 0, err
	}
	conversation, err := c.server.Store.GetActiveConversation(ctx, sandboxID)
	if errors.Is(err, store.ErrNotFound) {
		conversation = &store.Conversation{
			ID:          newULID(),
			SandboxID:   sandboxID,
			Agent:       store.ConversationAgentGitHubCopilot,
			DefaultMode: mode,
		}
		if err = c.server.Store.CreateConversation(ctx, conversation); err != nil {
			if !errors.Is(err, store.ErrConflict) {
				return nil, 0, err
			}
			conversation, err = c.server.Store.GetActiveConversation(ctx, sandboxID)
		}
	}
	if err != nil {
		return nil, 0, err
	}
	turn, position, err := c.server.Store.EnqueueConversationTurn(
		ctx, conversation.ID, newULID(), newULID(), prompt, mode,
		store.ConversationTurnSettings{
			Model: selection.Model, ReasoningEffort: selection.ReasoningEffort,
			ContextTier: selection.ContextTier,
		})
	if err != nil {
		return nil, 0, err
	}
	c.notify(conversation.ID)
	c.Start(conversation.ID)
	return turn, position, nil
}

// Reset archives an idle transcript, removes its remote SDK state, and creates
// a fresh current conversation for the same sandbox.
func (c *ConversationCoordinator) Reset(ctx context.Context, sandboxID, defaultMode string) (*store.Conversation, error) {
	if c == nil || c.server == nil || c.server.Copilot == nil {
		return nil, errConversationUnavailable
	}
	if !validConversationMode(defaultMode) {
		return nil, errInvalidConversation
	}
	if _, err := c.server.Store.Get(ctx, sandboxID); err != nil {
		return nil, err
	}
	previous, err := c.server.Store.ArchiveActiveConversation(ctx, sandboxID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if previous != nil {
		if err := c.server.Copilot.CleanupConversation(previous.ID); err != nil {
			return nil, err
		}
		c.notify(previous.ID)
	}
	conversation := &store.Conversation{
		ID: newULID(), SandboxID: sandboxID, Agent: store.ConversationAgentGitHubCopilot,
		DefaultMode: defaultMode,
	}
	if err := c.server.Store.CreateConversation(ctx, conversation); err != nil {
		return nil, err
	}
	c.notify(conversation.ID)
	return conversation, nil
}

// Start asynchronously drains a conversation's FIFO queue. At most one worker
// exists per conversation, including while it waits in a native SDK callback.
func (c *ConversationCoordinator) Start(conversationID string) {
	c.mu.Lock()
	if c.workers[conversationID] {
		c.mu.Unlock()
		return
	}
	c.workers[conversationID] = true
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.workers, conversationID)
			c.mu.Unlock()
			queued, err := c.server.Store.ConversationHasQueuedTurn(context.Background(), conversationID)
			if err != nil {
				c.logError("check queued conversation turn", err)
				return
			}
			if queued {
				c.Start(conversationID)
			}
		}()
		for {
			turn, err := c.server.Store.ClaimNextConversationTurn(context.Background(), conversationID)
			if errors.Is(err, store.ErrNotFound) {
				return
			}
			if errors.Is(err, store.ErrConflict) {
				if c.recoverOrphanedConversationTurn(conversationID) {
					continue
				}
				return
			}
			if err != nil {
				c.logError("claim conversation turn", err)
				return
			}
			c.notify(conversationID)
			c.execute(turn)
		}
	}()
}

func (c *ConversationCoordinator) execute(turn *store.ConversationTurn) {
	conversation, err := c.server.Store.GetConversation(context.Background(), turn.ConversationID)
	if err != nil {
		c.finishWithoutRuntime(turn, runtime.TaskFailed, "internal", "conversation no longer exists")
		return
	}
	run := &conversationRun{
		conversationID: conversation.ID,
		sandboxID:      conversation.SandboxID,
		turnID:         turn.ID,
		taskID:         turn.TaskID,
		budget:         newTurnBudget(runtime.DefaultTaskTimeout),
		waiters:        make(map[string]chan interactionDelivery),
	}
	c.setRun(run)
	defer func() {
		run.budget.Stop()
		run.setActive(c.server, false)
		c.clearRun(run)
	}()
	// A cancellation can race the tiny interval between the durable claim and
	// this in-memory callback registration. Once the run is registered, every
	// later cancellation reaches it directly; a prior one is terminalized here.
	current, err := c.server.Store.GetConversationTurn(context.Background(), turn.ID)
	if err != nil {
		c.finishWithoutRuntime(turn, runtime.TaskFailed, "internal", "Could not inspect the Copilot message state.")
		return
	}
	if current.Status == store.ConversationTurnCancelling {
		run.requestCancel()
		c.finishWithoutRuntime(turn, runtime.TaskCancelled, "cancelled", "")
		return
	}

	if err := c.ensureSandboxRunning(context.Background(), run.sandboxID); err != nil {
		c.finishWithoutRuntime(turn, runtime.TaskFailed, "sandbox_unavailable",
			"Sandbox is unavailable for this Copilot message.")
		return
	}
	prepareCtx, prepareCancel := context.WithTimeout(context.Background(), 45*time.Second)
	_, err = c.server.runtimeClientFor(run.sandboxID).PrepareHostedTask(prepareCtx,
		runtime.PrepareHostedTaskRequest{TaskID: run.taskID, Prompt: turn.Prompt})
	prepareCancel()
	if err != nil {
		reason, message := "sandbox_unavailable", "Sandbox is unavailable for this Copilot message."
		if errors.Is(err, runtime.ErrTaskInProgress) {
			reason, message = "task_in_progress", "Another sandbox task is still completing."
		}
		c.finishWithoutRuntime(turn, runtime.TaskFailed, reason, message)
		return
	}

	run.setActive(c.server, true)
	err = c.server.Copilot.RunConversationTurn(run.budget.Context(), copilot.ConversationTurnRequest{
		ConversationID:  conversation.ID,
		SandboxID:       run.sandboxID,
		Prompt:          turn.Prompt,
		Mode:            turn.Mode,
		Model:           turn.Model,
		ReasoningEffort: turn.ReasoningEffort,
		ContextTier:     turn.ContextTier,
		SystemPrompt: agentprompt.Render(agentprompt.Vars{
			AppDir: "/home/sandbox/workspace/app", Port: webPortOfMust(c.server, run.sandboxID),
			HealthPath: "/",
		}),
		OnEvent: func(event copilot.Envelope) { c.recordProviderEvent(run, event) },
		OnUserInput: func(input copilot.RuntimeInputRequest) (copilot.RuntimeInputResponse, error) {
			return c.waitForInput(run, input)
		},
		OnPlan: func(plan copilot.RuntimePlanRequest) (copilot.RuntimePlanResponse, error) {
			return c.waitForPlan(run, plan)
		},
	})
	status, reason, message := c.classifyOutcome(run, err)
	result := c.finalizeHostedTurn(turn, run, status, reason, message)
	if result.Status != status {
		status, reason, message = result.Status, result.FailureReason, result.ErrorMessage
	}
	raw, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		c.logError("encode hosted task result", marshalErr)
		raw = nil
		status, reason, message = runtime.TaskFailed, "internal", "Could not store the Copilot task result."
	}
	if err := c.server.Store.FinishConversationTurn(context.Background(), turn.ID, string(status), string(raw), message); err != nil &&
		!errors.Is(err, store.ErrConflict) {
		c.logError("finish conversation turn", err)
	}
	c.notify(turn.ConversationID)
}

func webPortOfMust(server *Server, sandboxID string) int {
	sb, err := server.Store.Get(context.Background(), sandboxID)
	if err != nil {
		return 3000
	}
	return webPortOf(sb)
}

func (c *ConversationCoordinator) finalizeHostedTurn(turn *store.ConversationTurn, run *conversationRun, status runtime.TaskStatus, reason, message string) runtime.TaskResult {
	finalizeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result, err := c.server.runtimeClientFor(run.sandboxID).FinalizeHostedTask(finalizeCtx,
		runtime.FinalizeHostedTaskRequest{
			TaskID:            turn.TaskID,
			Status:            status,
			FailureReason:     reason,
			ErrorMessage:      message,
			AgentMessageFinal: run.assistantText(),
			Tokens:            run.tokenUsage(),
		})
	if err == nil {
		return *result
	}
	c.logError("finalize hosted Copilot task", err)
	now := time.Now().UTC()
	return runtime.TaskResult{
		ID:                turn.TaskID,
		Prompt:            turn.Prompt,
		Status:            runtime.TaskFailed,
		FailureReason:     "sandbox_unavailable",
		ErrorMessage:      "Sandbox was unavailable while finalizing the Copilot message.",
		FilesChanged:      []string{},
		AgentMessageFinal: run.assistantText(),
		BuildStatus:       runtime.BuildSkipped,
		CreatedAt:         turn.CreatedAt,
		StartedAt:         timeFromNullUnix(turn.StartedAt, turn.CreatedAt),
		FinishedAt:        now,
		DurationMS:        now.Sub(timeFromNullUnix(turn.StartedAt, turn.CreatedAt)).Milliseconds(),
		Tokens:            run.tokenUsage(),
	}
}

// recoverInterruptedTurn releases runtimed's persisted hosted-task marker
// before changing the durable conversation to terminal. That order prevents a
// control-plane restart from stranding an old hosted task and blocking every
// later task in the sandbox.
func (c *ConversationCoordinator) recoverInterruptedTurn(ctx context.Context, turn *store.ConversationTurn, message string) error {
	conversation, err := c.server.Store.GetConversation(ctx, turn.ConversationID)
	if err != nil {
		return err
	}
	if err := c.ensureSandboxRunning(ctx, conversation.SandboxID); err != nil {
		return fmt.Errorf("wake sandbox for interrupted hosted task: %w", err)
	}

	finalizeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	result, finalizeErr := c.server.runtimeClientFor(conversation.SandboxID).FinalizeHostedTask(finalizeCtx,
		runtime.FinalizeHostedTaskRequest{
			TaskID:        turn.TaskID,
			Status:        runtime.TaskFailed,
			FailureReason: "provider_interrupted",
			ErrorMessage:  message,
		})
	cancel()
	if finalizeErr != nil {
		c.logError("finalize interrupted hosted task", finalizeErr)
		abandonCtx, abandonCancel := context.WithTimeout(context.Background(), 30*time.Second)
		result, err = c.server.runtimeClientFor(conversation.SandboxID).AbandonHostedTask(abandonCtx,
			runtime.AbandonHostedTaskRequest{
				TaskID:        turn.TaskID,
				Status:        runtime.TaskFailed,
				FailureReason: "provider_interrupted",
				ErrorMessage:  message,
			})
		abandonCancel()
		if err != nil {
			return fmt.Errorf("abandon interrupted hosted task: %w", err)
		}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode recovered hosted task result: %w", err)
	}
	if err := c.server.Store.FinishConversationTurn(ctx, turn.ID, string(result.Status), string(raw), result.ErrorMessage); err != nil &&
		!errors.Is(err, store.ErrConflict) {
		return err
	}
	return nil
}

// recoverOrphanedConversationTurn gives a later queued submission a bounded
// chance to clean up a restart-recovery failure once its sandbox is available.
func (c *ConversationCoordinator) recoverOrphanedConversationTurn(conversationID string) bool {
	c.mu.Lock()
	run := c.runs[conversationID]
	c.mu.Unlock()
	if run != nil {
		return false
	}
	conversation, err := c.server.Store.GetConversation(context.Background(), conversationID)
	if err != nil || !conversation.ActiveTurnID.Valid {
		return errors.Is(err, store.ErrNotFound)
	}
	turn, err := c.server.Store.GetConversationTurn(context.Background(), conversation.ActiveTurnID.String)
	if err != nil {
		c.logError("read orphaned conversation turn", err)
		return false
	}
	if err := c.recoverInterruptedTurn(context.Background(), turn,
		"Copilot was interrupted before it could finish. Send a new message to continue."); err != nil {
		c.logError("retry interrupted conversation recovery", err)
		return false
	}
	c.notify(conversationID)
	return true
}

func (c *ConversationCoordinator) finishWithoutRuntime(turn *store.ConversationTurn, status runtime.TaskStatus, reason, message string) {
	now := time.Now().UTC()
	result := runtime.TaskResult{
		ID: turn.TaskID, Prompt: turn.Prompt, Status: status, FailureReason: reason,
		ErrorMessage: message, FilesChanged: []string{}, BuildStatus: runtime.BuildSkipped,
		CreatedAt: turn.CreatedAt, StartedAt: timeFromNullUnix(turn.StartedAt, turn.CreatedAt),
		FinishedAt: now, DurationMS: now.Sub(timeFromNullUnix(turn.StartedAt, turn.CreatedAt)).Milliseconds(),
	}
	raw, _ := json.Marshal(result)
	if err := c.server.Store.FinishConversationTurn(context.Background(), turn.ID, string(status), string(raw), message); err != nil &&
		!errors.Is(err, store.ErrConflict) {
		c.logError("finish unavailable conversation turn", err)
	}
	c.notify(turn.ConversationID)
}

func timeFromNullUnix(value sql.NullInt64, fallback time.Time) time.Time {
	if value.Valid {
		return time.Unix(value.Int64, 0).UTC()
	}
	return fallback
}

func (c *ConversationCoordinator) classifyOutcome(run *conversationRun, err error) (runtime.TaskStatus, string, string) {
	if err == nil {
		return runtime.TaskSucceeded, "", ""
	}
	if run.wasCancelled() {
		return runtime.TaskCancelled, "cancelled", ""
	}
	if run.budget.Expired() || errors.Is(err, context.DeadlineExceeded) {
		return runtime.TaskFailed, "agent_timeout", "Copilot did not finish within the active execution limit."
	}
	if errors.Is(err, context.Canceled) {
		return runtime.TaskFailed, "provider_interrupted", "Copilot stopped before completing this message."
	}
	return runtime.TaskFailed, "agent_error", "Copilot could not complete this message."
}

func (c *ConversationCoordinator) recordProviderEvent(run *conversationRun, event copilot.Envelope) {
	ctx := context.Background()
	switch event.Type {
	case "message":
		if err := c.server.Store.AppendAssistantText(ctx, run.conversationID, run.turnID,
			run.turnID+"-assistant", event.Text); err != nil {
			c.logError("persist Copilot message", err)
			return
		}
		run.appendAssistant(event.Text)
	case "tool":
		if err := c.server.Store.AppendConversationEvent(ctx, run.conversationID, run.turnID,
			"tool", map[string]any{"name": event.Name, "status": event.Status}); err != nil {
			c.logError("persist Copilot tool event", err)
			return
		}
	case "usage":
		run.setTokens(event)
		if err := c.server.Store.AppendConversationEvent(ctx, run.conversationID, run.turnID,
			"usage", map[string]any{
				"input": event.Input, "output": event.Output, "reasoning": event.Reasoning,
				"cache_read": event.CacheRead, "cache_write": event.CacheWrite, "total": event.Total,
			}); err != nil {
			c.logError("persist Copilot usage", err)
			return
		}
	default:
		return
	}
	c.notify(run.conversationID)
}

func (c *ConversationCoordinator) waitForInput(run *conversationRun, request copilot.RuntimeInputRequest) (copilot.RuntimeInputResponse, error) {
	if err := validInteractionInput(request.Question, request.Choices); err != nil {
		return copilot.RuntimeInputResponse{}, err
	}
	interaction := &store.ConversationInteraction{
		ID: newULID(), ConversationID: run.conversationID, TurnID: run.turnID,
		Type: store.ConversationInteractionInput, ProviderRequestID: request.RequestID,
		Question: request.Question, Choices: append([]string(nil), request.Choices...),
		AllowFreeform: request.AllowFreeform,
	}
	waiter := run.installWaiter(interaction.ID)
	run.pause(c.server)
	if err := c.server.Store.CreateConversationInteraction(context.Background(), interaction); err != nil {
		run.removeWaiter(interaction.ID)
		run.resume(c.server)
		return copilot.RuntimeInputResponse{}, err
	}
	c.notify(run.conversationID)
	defer run.removeWaiter(interaction.ID)

	select {
	case response := <-waiter:
		run.resume(c.server)
		return copilot.RuntimeInputResponse{Answer: response.answer, WasFreeform: !contains(request.Choices, response.answer)}, nil
	case <-run.budget.Context().Done():
		return copilot.RuntimeInputResponse{}, run.budget.Context().Err()
	}
}

func (c *ConversationCoordinator) waitForPlan(run *conversationRun, request copilot.RuntimePlanRequest) (copilot.RuntimePlanResponse, error) {
	if err := validInteractionPlan(request); err != nil {
		return copilot.RuntimePlanResponse{}, err
	}
	interaction := &store.ConversationInteraction{
		ID: newULID(), ConversationID: run.conversationID, TurnID: run.turnID,
		Type: store.ConversationInteractionPlan, ProviderRequestID: request.RequestID,
		Summary: request.Summary, Plan: request.Plan, Actions: append([]string(nil), request.Actions...),
		RecommendedAction: request.RecommendedAction,
	}
	waiter := run.installWaiter(interaction.ID)
	run.pause(c.server)
	if err := c.server.Store.CreateConversationInteraction(context.Background(), interaction); err != nil {
		run.removeWaiter(interaction.ID)
		run.resume(c.server)
		return copilot.RuntimePlanResponse{}, err
	}
	c.notify(run.conversationID)
	defer run.removeWaiter(interaction.ID)

	select {
	case response := <-waiter:
		run.resume(c.server)
		return copilot.RuntimePlanResponse{
			Approved: response.approved, SelectedAction: response.selectedAction, Feedback: response.feedback,
		}, nil
	case <-run.budget.Context().Done():
		return copilot.RuntimePlanResponse{}, run.budget.Context().Err()
	}
}

// Answer persists and delivers one native callback response exactly once. A
// stopped sandbox is woken before the provider can resume workspace operations.
func (c *ConversationCoordinator) Answer(ctx context.Context, sandboxID, interactionID, answer string, approved *bool, selectedAction, feedback string) (*store.ConversationInteraction, error) {
	interaction, err := c.server.Store.GetConversationInteraction(ctx, interactionID)
	if err != nil {
		return nil, err
	}
	conversation, err := c.server.Store.GetConversation(ctx, interaction.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation.SandboxID != sandboxID || conversation.ArchivedAt.Valid {
		return nil, store.ErrNotFound
	}
	if interaction.Status != store.ConversationInteractionPending {
		return nil, store.ErrConflict
	}
	if interaction.Type == store.ConversationInteractionInput {
		if approved != nil || selectedAction != "" || feedback != "" || !validInputAnswer(interaction, answer) {
			return nil, errInvalidConversation
		}
	} else if interaction.Type == store.ConversationInteractionPlan {
		if approved == nil || answer != "" || !validPlanAnswer(interaction, *approved, selectedAction, feedback) {
			return nil, errInvalidConversation
		}
	} else {
		return nil, errInvalidConversation
	}
	if err := c.ensureSandboxRunning(ctx, sandboxID); err != nil {
		return nil, fmt.Errorf("%w: %v", errSandboxUnavailable, err)
	}

	c.mu.Lock()
	run := c.runs[conversation.ID]
	c.mu.Unlock()
	if run == nil {
		return nil, errInteractionUnavailable
	}
	delivery := interactionDelivery{
		answer: answer, selectedAction: selectedAction, feedback: feedback,
		isPlan: interaction.Type == store.ConversationInteractionPlan,
	}
	if approved != nil {
		delivery.approved = *approved
	}
	resolved, err := run.resolveAndDeliver(ctx, c.server.Store, interactionID, delivery)
	if err != nil {
		return nil, err
	}
	c.notify(conversation.ID)
	return resolved, nil
}

// Cancel makes the in-flight callback return and lets the normal finalization
// path write the canonical cancelled task result.
func (c *ConversationCoordinator) Cancel(ctx context.Context, sandboxID string) (*store.ConversationTurn, error) {
	conversation, err := c.server.Store.GetActiveConversation(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	turn, err := c.server.Store.RequestConversationTurnCancellation(ctx, conversation.ID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	run := c.runs[conversation.ID]
	c.mu.Unlock()
	if run == nil {
		if c.hasWorker(conversation.ID) {
			c.notify(conversation.ID)
			return turn, nil
		}
		now := time.Now().UTC()
		result := runtime.TaskResult{
			ID: turn.TaskID, Prompt: turn.Prompt, Status: runtime.TaskCancelled,
			FilesChanged: []string{}, BuildStatus: runtime.BuildSkipped,
			CreatedAt: turn.CreatedAt, StartedAt: timeFromNullUnix(turn.StartedAt, turn.CreatedAt),
			FinishedAt: now, DurationMS: now.Sub(timeFromNullUnix(turn.StartedAt, turn.CreatedAt)).Milliseconds(),
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		if err := c.server.Store.FinishConversationTurn(ctx, turn.ID, string(runtime.TaskCancelled), string(raw), ""); err != nil {
			return nil, err
		}
		finished, err := c.server.Store.GetConversationTurn(ctx, turn.ID)
		if err != nil {
			return nil, err
		}
		c.notify(conversation.ID)
		return finished, nil
	}
	run.requestCancel()
	c.server.Copilot.CancelConversation(conversation.ID)
	c.notify(conversation.ID)
	return turn, nil
}

// InterruptSandbox aborts active callbacks before irreversible sandbox cleanup.
func (c *ConversationCoordinator) InterruptSandbox(ctx context.Context, sandboxID, message string) {
	if c == nil || c.server == nil || c.server.Store == nil {
		return
	}
	ids, err := c.server.Store.ListConversationIDsForSandbox(ctx, sandboxID)
	if err != nil {
		c.logError("list sandbox conversations", err)
		return
	}
	for _, id := range ids {
		c.interruptConversation(ctx, id, message, false)
	}
}

// InterruptAll is used after the credential manager rotates or removes the
// provider credential, when every native SDK callback becomes invalid.
func (c *ConversationCoordinator) InterruptAll(ctx context.Context, message string) {
	if c == nil || c.server == nil || c.server.Store == nil {
		return
	}
	turns, err := c.server.Store.ListActiveConversationTurns(ctx)
	if err != nil {
		c.logError("list active conversations", err)
		return
	}
	for _, turn := range turns {
		c.interruptConversation(ctx, turn.ConversationID, message, true)
	}
}

func (c *ConversationCoordinator) interruptConversation(ctx context.Context, conversationID, message string, finalizeOrphan bool) {
	conversation, err := c.server.Store.GetConversation(ctx, conversationID)
	if err != nil {
		return
	}
	c.mu.Lock()
	run := c.runs[conversationID]
	c.mu.Unlock()
	if run != nil {
		run.requestCancel()
	}
	if c.server.Copilot != nil {
		c.server.Copilot.CancelConversation(conversationID)
	}
	if !conversation.ActiveTurnID.Valid {
		c.notify(conversationID)
		return
	}
	if run != nil {
		// The live worker owns the normal hosted finalization path. Marking the
		// turn terminal here would race that cleanup and leave stale metadata if
		// the process exits before the worker can finish.
		c.notify(conversationID)
		return
	}
	turn, err := c.server.Store.GetConversationTurn(ctx, conversation.ActiveTurnID.String)
	if err != nil {
		c.logError("read interrupted conversation turn", err)
		c.notify(conversationID)
		return
	}
	if finalizeOrphan {
		if err := c.recoverInterruptedTurn(ctx, turn, message); err != nil {
			c.logError("finalize interrupted conversation", err)
		}
	} else if err := c.server.Store.InterruptConversationTurn(ctx, turn.ID, message); err != nil &&
		!errors.Is(err, store.ErrConflict) {
		c.logError("interrupt conversation", err)
	}
	c.notify(conversationID)
}

func (c *ConversationCoordinator) ensureSandboxRunning(ctx context.Context, sandboxID string) error {
	sb, err := c.server.Store.Get(ctx, sandboxID)
	if err != nil {
		return err
	}
	switch sb.Status {
	case "running":
		return nil
	case "stopped":
		outer := httptest.NewRequest(http.MethodPost, "/wake/"+sandboxID, nil).WithContext(ctx)
		code, body := c.server.delegate(outer, c.server.handleWakeJSON, http.MethodPost,
			"/wake/"+sandboxID, map[string]string{"id": sandboxID}, nil)
		if code == http.StatusOK {
			return nil
		}
		var response struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &response)
		if response.Error == "" {
			response.Error = "sandbox could not be started"
		}
		return errors.New(response.Error)
	default:
		return fmt.Errorf("sandbox is %s", sb.Status)
	}
}

func (c *ConversationCoordinator) setRun(run *conversationRun) {
	c.mu.Lock()
	c.runs[run.conversationID] = run
	c.mu.Unlock()
}

func (c *ConversationCoordinator) clearRun(run *conversationRun) {
	c.mu.Lock()
	if c.runs[run.conversationID] == run {
		delete(c.runs, run.conversationID)
	}
	c.mu.Unlock()
}

func (c *ConversationCoordinator) hasWorker(conversationID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.workers[conversationID]
}

func (c *ConversationCoordinator) subscribe(conversationID string) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	channel := c.updates[conversationID]
	if channel == nil {
		channel = make(chan struct{})
		c.updates[conversationID] = channel
	}
	return channel
}

func (c *ConversationCoordinator) notify(conversationID string) {
	c.mu.Lock()
	channel := c.updates[conversationID]
	if channel == nil {
		c.updates[conversationID] = make(chan struct{})
		c.mu.Unlock()
		return
	}
	close(channel)
	c.updates[conversationID] = make(chan struct{})
	c.mu.Unlock()
}

func (c *ConversationCoordinator) logError(operation string, err error) {
	if err == nil || c.server == nil || c.server.Log == nil {
		return
	}
	c.server.Log.Error("Copilot conversation: "+operation, "err", err.Error())
}

func (r *conversationRun) installWaiter(interactionID string) <-chan interactionDelivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	waiter := make(chan interactionDelivery, 1)
	r.waiters[interactionID] = waiter
	return waiter
}

func (r *conversationRun) removeWaiter(interactionID string) {
	r.mu.Lock()
	delete(r.waiters, interactionID)
	r.mu.Unlock()
}

func (r *conversationRun) resolveAndDeliver(ctx context.Context, st *store.Store, interactionID string, delivery interactionDelivery) (*store.ConversationInteraction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	waiter := r.waiters[interactionID]
	if waiter == nil {
		return nil, errInteractionUnavailable
	}
	var approved *bool
	if delivery.isPlan {
		value := delivery.approved
		approved = &value
	}
	resolved, err := st.ResolveConversationInteraction(ctx, interactionID, delivery.answer, approved,
		delivery.selectedAction, delivery.feedback)
	if err != nil {
		return nil, err
	}
	select {
	case waiter <- delivery:
		return resolved, nil
	default:
		return nil, errInteractionUnavailable
	}
}

func (r *conversationRun) pause(server *Server) {
	r.budget.Pause()
	r.setActive(server, false)
}

func (r *conversationRun) resume(server *Server) {
	r.budget.Resume()
	r.setActive(server, true)
}

func (r *conversationRun) setActive(server *Server, active bool) {
	r.mu.Lock()
	if r.active == active {
		r.mu.Unlock()
		return
	}
	r.active = active
	r.mu.Unlock()
	if server == nil || server.Inflight == nil {
		return
	}
	if active {
		server.Inflight.Enter(r.sandboxID)
	} else {
		server.Inflight.Exit(r.sandboxID)
	}
}

func (r *conversationRun) requestCancel() {
	r.mu.Lock()
	r.cancelled = true
	r.mu.Unlock()
	r.budget.Cancel()
}

func (r *conversationRun) wasCancelled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled
}

func (r *conversationRun) appendAssistant(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.assistant)+len(text) > conversationPromptLimit {
		return
	}
	r.assistant += text
}

func (r *conversationRun) assistantText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.assistant
}

func (r *conversationRun) setTokens(event copilot.Envelope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens = runtime.TokenUsage{
		Input: clampToken(event.Input), Output: clampToken(event.Output), Reasoning: clampToken(event.Reasoning),
		CacheRead: clampToken(event.CacheRead), CacheWrite: clampToken(event.CacheWrite), Total: clampToken(event.Total),
	}
}

func (r *conversationRun) tokenUsage() runtime.TokenUsage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tokens
}

func validConversationMode(mode string) bool {
	switch mode {
	case store.ConversationModeInteractive, store.ConversationModePlan, store.ConversationModeAutopilot:
		return true
	default:
		return false
	}
}

func validInteractionInput(question string, choices []string) error {
	if question == "" || len(question) > conversationInteractionLimit || len(choices) > 50 {
		return errors.New("invalid Copilot input request")
	}
	for _, choice := range choices {
		if choice == "" || len(choice) > 4096 {
			return errors.New("invalid Copilot input choice")
		}
	}
	return nil
}

func validInteractionPlan(request copilot.RuntimePlanRequest) error {
	if len(request.Summary) > conversationInteractionLimit || len(request.Plan) > conversationInteractionLimit ||
		len(request.Actions) > 10 || len(request.RecommendedAction) > 128 {
		return errors.New("invalid Copilot plan request")
	}
	for _, action := range request.Actions {
		if action == "" || len(action) > 128 {
			return errors.New("invalid Copilot plan action")
		}
	}
	return nil
}

func validInputAnswer(interaction *store.ConversationInteraction, answer string) bool {
	if answer == "" || len(answer) > conversationInteractionLimit {
		return false
	}
	return interaction.AllowFreeform || contains(interaction.Choices, answer)
}

func validPlanAnswer(interaction *store.ConversationInteraction, approved bool, selectedAction, feedback string) bool {
	if len(feedback) > conversationInteractionLimit {
		return false
	}
	if !approved {
		return selectedAction == ""
	}
	return selectedAction != "" && contains(interaction.Actions, selectedAction)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func clampToken(value int64) int {
	if value <= 0 {
		return 0
	}
	max := int(^uint(0) >> 1)
	if uint64(value) > uint64(max) {
		return max
	}
	return int(value)
}
