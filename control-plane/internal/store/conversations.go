package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtime"
)

const (
	ConversationAgentGitHubCopilot = "github-copilot"

	ConversationModeInteractive = "interactive"
	ConversationModePlan        = "plan"
	ConversationModeAutopilot   = "autopilot"

	ConversationStateIdle         = "idle"
	ConversationStateRunning      = "running"
	ConversationStateWaitingInput = "waiting_input"
	ConversationStateWaitingPlan  = "waiting_plan"
	ConversationStateFailed       = "failed"
	ConversationStateArchived     = "archived"

	ConversationTurnQueued       = "queued"
	ConversationTurnRunning      = "running"
	ConversationTurnWaitingInput = "waiting_input"
	ConversationTurnWaitingPlan  = "waiting_plan"
	ConversationTurnCancelling   = "cancelling"
	ConversationTurnSucceeded    = "succeeded"
	ConversationTurnFailed       = "failed"
	ConversationTurnCancelled    = "cancelled"

	ConversationInteractionInput = "user_input"
	ConversationInteractionPlan  = "plan"

	ConversationInteractionPending     = "pending"
	ConversationInteractionResolved    = "resolved"
	ConversationInteractionInterrupted = "interrupted"

	conversationMessageStreaming = "streaming"
	conversationMessageComplete  = "complete"

	maxConversationEventPayload = 64 << 10
	maxConversationMessageBytes = 1 << 20
)

// Conversation is one durable hosted-provider context for a sandbox. SDK
// session IDs intentionally do not live here: the encrypted Copilot manager
// stores them by this opaque conversation ID.
type Conversation struct {
	ID           string
	SandboxID    string
	Agent        string
	State        string
	DefaultMode  string
	ActiveTurnID sql.NullString
	LastError    sql.NullString
	NextSequence int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ArchivedAt   sql.NullInt64
}

// ConversationTurn is one queued or completed user request within a
// conversation. TaskID links to the existing checkpoint/result/revert flow.
type ConversationTurn struct {
	ID              string
	ConversationID  string
	TaskID          string
	Sequence        int64
	Prompt          string
	Mode            string
	Model           string
	ReasoningEffort string
	ContextTier     string
	Status          string
	ErrorMessage    sql.NullString
	CreatedAt       time.Time
	StartedAt       sql.NullInt64
	FinishedAt      sql.NullInt64
}

// ConversationTurnSettings captures the immutable provider settings resolved
// when a user queues a turn. They must never be read from current live settings
// at execution time because a later settings update must not change queued work.
type ConversationTurnSettings struct {
	Model           string
	ReasoningEffort string
	ContextTier     string
}

// ConversationMessage is an ordered user or assistant transcript item.
type ConversationMessage struct {
	ID             string
	ConversationID string
	TurnID         string
	Sequence       int64
	Role           string
	Content        string
	Status         string
	CreatedAt      time.Time
}

// ConversationInteraction records a provider request for user input or a plan
// approval. ProviderRequestID is never sent over the public API.
type ConversationInteraction struct {
	ID                string
	ConversationID    string
	TurnID            string
	Sequence          int64
	Type              string
	Status            string
	ProviderRequestID string `json:"-"`
	Question          string
	Choices           []string
	AllowFreeform     bool
	Summary           string
	Plan              string
	Actions           []string
	RecommendedAction string
	Answer            sql.NullString
	Approved          sql.NullBool
	SelectedAction    sql.NullString
	Feedback          sql.NullString
	CreatedAt         time.Time
	ResolvedAt        sql.NullInt64
}

// ConversationEvent is a durable, redacted SSE event. Its ID is a per-store
// monotonic replay cursor; clients scope it by ConversationID.
type ConversationEvent struct {
	ID             int64
	ConversationID string
	TurnID         sql.NullString
	Type           string
	Payload        json.RawMessage
	CreatedAt      time.Time
}

// ConversationSnapshot is the console's durable initial state. The caller
// reconnects its event stream after EventCursor to avoid a snapshot race.
type ConversationSnapshot struct {
	Conversation  *Conversation
	Turns         []*ConversationTurn
	Messages      []*ConversationMessage
	Interactions  []*ConversationInteraction
	Children      []*ConversationChild
	EventCursor   int64
	NextQueueSlot int
}

const conversationSelectCols = `id, sandbox_id, agent, state, default_mode,
	active_turn_id, last_error, next_sequence, created_at, updated_at, archived_at`

const conversationTurnSelectCols = `id, conversation_id, task_id, sequence,
	prompt, mode, model, reasoning_effort, context_tier, status, error_message,
	created_at, started_at, finished_at`

const conversationMessageSelectCols = `id, conversation_id, turn_id, sequence,
	role, content, status, created_at`

const conversationInteractionSelectCols = `id, conversation_id, turn_id,
	sequence, type, status, provider_request_id, question, choices_json,
	allow_freeform, summary, plan, actions_json, recommended_action, answer,
	approved, selected_action, feedback, created_at, resolved_at`

func scanConversation(sc scanner) (*Conversation, error) {
	var c Conversation
	var created, updated int64
	if err := sc.Scan(&c.ID, &c.SandboxID, &c.Agent, &c.State, &c.DefaultMode,
		&c.ActiveTurnID, &c.LastError, &c.NextSequence, &created, &updated,
		&c.ArchivedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.CreatedAt = time.Unix(created, 0).UTC()
	c.UpdatedAt = time.Unix(updated, 0).UTC()
	return &c, nil
}

func scanConversationTurn(sc scanner) (*ConversationTurn, error) {
	var turn ConversationTurn
	var created int64
	if err := sc.Scan(&turn.ID, &turn.ConversationID, &turn.TaskID, &turn.Sequence,
		&turn.Prompt, &turn.Mode, &turn.Model, &turn.ReasoningEffort,
		&turn.ContextTier, &turn.Status, &turn.ErrorMessage, &created,
		&turn.StartedAt, &turn.FinishedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	turn.CreatedAt = time.Unix(created, 0).UTC()
	return &turn, nil
}

func scanConversationMessage(sc scanner) (*ConversationMessage, error) {
	var message ConversationMessage
	var created int64
	if err := sc.Scan(&message.ID, &message.ConversationID, &message.TurnID,
		&message.Sequence, &message.Role, &message.Content, &message.Status,
		&created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	message.CreatedAt = time.Unix(created, 0).UTC()
	return &message, nil
}

func scanConversationInteraction(sc scanner) (*ConversationInteraction, error) {
	var interaction ConversationInteraction
	var choices, actions string
	var allowFreeform int
	var created int64
	if err := sc.Scan(&interaction.ID, &interaction.ConversationID, &interaction.TurnID,
		&interaction.Sequence, &interaction.Type, &interaction.Status,
		&interaction.ProviderRequestID, &interaction.Question, &choices,
		&allowFreeform, &interaction.Summary, &interaction.Plan, &actions,
		&interaction.RecommendedAction, &interaction.Answer, &interaction.Approved,
		&interaction.SelectedAction, &interaction.Feedback, &created,
		&interaction.ResolvedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(choices), &interaction.Choices)
	_ = json.Unmarshal([]byte(actions), &interaction.Actions)
	if interaction.Choices == nil {
		interaction.Choices = []string{}
	}
	if interaction.Actions == nil {
		interaction.Actions = []string{}
	}
	interaction.AllowFreeform = allowFreeform != 0
	interaction.CreatedAt = time.Unix(created, 0).UTC()
	return &interaction, nil
}

func scanConversationEvent(sc scanner) (*ConversationEvent, error) {
	var event ConversationEvent
	var raw string
	var created int64
	if err := sc.Scan(&event.ID, &event.ConversationID, &event.TurnID, &event.Type,
		&raw, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	event.Payload = json.RawMessage(raw)
	event.CreatedAt = time.Unix(created, 0).UTC()
	return &event, nil
}

// CreateConversation creates a fresh active conversation. The partial unique
// index rejects a racing second current conversation for the same sandbox.
func (s *Store) CreateConversation(ctx context.Context, c *Conversation) error {
	if c == nil || c.ID == "" || c.SandboxID == "" {
		return errors.New("invalid conversation")
	}
	if c.Agent == "" {
		c.Agent = ConversationAgentGitHubCopilot
	}
	if c.State == "" {
		c.State = ConversationStateIdle
	}
	if c.DefaultMode == "" {
		c.DefaultMode = ConversationModeInteractive
	}
	return s.submit(ctx, func(db *sql.DB) error {
		now := time.Now().Unix()
		_, err := db.ExecContext(ctx, `
			INSERT INTO conversation
			    (id, sandbox_id, agent, state, default_mode, active_turn_id,
			     last_error, next_sequence, created_at, updated_at, archived_at)
			VALUES (?, ?, ?, ?, ?, NULL, NULL, 0, ?, ?, NULL)`,
			c.ID, c.SandboxID, c.Agent, c.State, c.DefaultMode, now, now)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		c.CreatedAt = time.Unix(now, 0).UTC()
		c.UpdatedAt = c.CreatedAt
		return nil
	})
}

// GetConversation returns a conversation regardless of archival state.
func (s *Store) GetConversation(ctx context.Context, id string) (*Conversation, error) {
	return scanConversation(s.db.QueryRowContext(ctx,
		`SELECT `+conversationSelectCols+` FROM conversation WHERE id=?`, id))
}

// GetActiveConversation returns the current conversation for a sandbox.
func (s *Store) GetActiveConversation(ctx context.Context, sandboxID string) (*Conversation, error) {
	return getActiveConversation(ctx, s.db, sandboxID)
}

type conversationQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getActiveConversation(ctx context.Context, queryer conversationQueryer, sandboxID string) (*Conversation, error) {
	return scanConversation(queryer.QueryRowContext(ctx,
		`SELECT `+conversationSelectCols+`
		   FROM conversation
		  WHERE sandbox_id=? AND archived_at IS NULL
		  ORDER BY created_at DESC LIMIT 1`, sandboxID))
}

// ListConversationIDsForSandbox returns current and archived conversation IDs.
// Purge uses this before deleting rows so the Copilot manager can delete its
// external SDK sessions.
func (s *Store) ListConversationIDsForSandbox(ctx context.Context, sandboxID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM conversation WHERE sandbox_id=?`, sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ArchiveActiveConversation closes an idle conversation so a new one can be
// made. It deliberately refuses a queued, running, or waiting turn.
func (s *Store) ArchiveActiveConversation(ctx context.Context, sandboxID string) (*Conversation, error) {
	var archived *Conversation
	err := s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		current, err := scanConversation(tx.QueryRowContext(ctx,
			`SELECT `+conversationSelectCols+`
			   FROM conversation WHERE sandbox_id=? AND archived_at IS NULL
			   ORDER BY created_at DESC LIMIT 1`, sandboxID))
		if err != nil {
			return err
		}
		var queued int
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM conversation_turn
			                WHERE conversation_id=? AND status='queued')`, current.ID).Scan(&queued); err != nil {
			return err
		}
		if current.ActiveTurnID.Valid || queued != 0 {
			return ErrConflict
		}
		now := time.Now().Unix()
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversation
			   SET state=?, archived_at=?, updated_at=?
			 WHERE id=?`, ConversationStateArchived, now, now, current.ID); err != nil {
			return err
		}
		current.State = ConversationStateArchived
		current.ArchivedAt = sql.NullInt64{Int64: now, Valid: true}
		current.UpdatedAt = time.Unix(now, 0).UTC()
		archived = current
		return tx.Commit()
	})
	return archived, err
}

// EnqueueConversationTurn atomically creates the user transcript message, its
// task row, and a FIFO turn. The caller chooses opaque IDs so retries cannot
// accidentally duplicate an otherwise valid turn.
func (s *Store) EnqueueConversationTurn(ctx context.Context, conversationID, turnID, taskID, prompt, mode string, settings ConversationTurnSettings) (*ConversationTurn, int, error) {
	if conversationID == "" || turnID == "" || taskID == "" || prompt == "" {
		return nil, 0, errors.New("invalid conversation turn")
	}
	if settings.ContextTier == "" {
		settings.ContextTier = "default"
	}
	if len(settings.Model) > 256 || len(settings.ReasoningEffort) > 64 ||
		len(settings.ContextTier) > 64 {
		return nil, 0, errors.New("invalid conversation turn settings")
	}
	var (
		turn     *ConversationTurn
		position int
	)
	err := s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		conversation, err := scanConversation(tx.QueryRowContext(ctx,
			`SELECT `+conversationSelectCols+` FROM conversation WHERE id=?`, conversationID))
		if err != nil {
			return err
		}
		if conversation.ArchivedAt.Valid {
			return ErrConflict
		}
		var externalUserID, externalProjectID sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT external_user_id, external_project_id FROM sandbox WHERE id=?`,
			conversation.SandboxID).Scan(&externalUserID, &externalProjectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		sequence, err := nextConversationSequence(ctx, tx, conversation.ID)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversation_turn
			    (id, conversation_id, task_id, sequence, prompt, mode, model,
			     reasoning_effort, context_tier, status, error_message, created_at,
			     started_at, finished_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, NULL, NULL)`,
			turnID, conversation.ID, taskID, sequence, prompt, mode,
			settings.Model, settings.ReasoningEffort, settings.ContextTier,
			ConversationTurnQueued, now); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO task
			    (task_id, sandbox_id, external_user_id, external_project_id,
			     agent, prompt, status, timeout_s, created_at, execution_kind,
			     conversation_id, conversation_turn_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
			taskID, conversation.SandboxID, externalUserID, externalProjectID,
			ConversationAgentGitHubCopilot, prompt, ConversationTurnQueued, now,
			TaskExecutionHostedCopilot, conversation.ID, turnID); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		messageID := turnID + "-user"
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversation_message
			    (id, conversation_id, turn_id, sequence, role, content, status, created_at)
			VALUES (?, ?, ?, ?, 'user', ?, ?, ?)`,
			messageID, conversation.ID, turnID, sequence, prompt,
			conversationMessageComplete, now); err != nil {
			return err
		}
		if _, err := appendConversationEvent(ctx, tx, conversation.ID, turnID,
			"message.created", map[string]any{
				"id": messageID, "turn_id": turnID, "sequence": sequence,
				"role": "user", "content": prompt, "status": conversationMessageComplete,
			}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE conversation SET updated_at=?, default_mode=? WHERE id=?`,
			now, mode, conversation.ID); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM conversation_turn
			 WHERE conversation_id=? AND status='queued' AND sequence <= ?`,
			conversation.ID, sequence).Scan(&position); err != nil {
			return err
		}
		turn = &ConversationTurn{
			ID: turnID, ConversationID: conversation.ID, TaskID: taskID,
			Sequence: sequence, Prompt: prompt, Mode: mode, Model: settings.Model,
			ReasoningEffort: settings.ReasoningEffort, ContextTier: settings.ContextTier,
			Status:    ConversationTurnQueued,
			CreatedAt: time.Unix(now, 0).UTC(),
		}
		return tx.Commit()
	})
	return turn, position, err
}

// ClaimNextConversationTurn transitions the oldest queued turn to active. A
// missing turn means the conversation is idle; ErrConflict means another live
// turn still owns the conversation.
func (s *Store) ClaimNextConversationTurn(ctx context.Context, conversationID string) (*ConversationTurn, error) {
	var turn *ConversationTurn
	err := s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		conversation, err := scanConversation(tx.QueryRowContext(ctx,
			`SELECT `+conversationSelectCols+` FROM conversation WHERE id=?`, conversationID))
		if err != nil {
			return err
		}
		if conversation.ArchivedAt.Valid || conversation.ActiveTurnID.Valid {
			return ErrConflict
		}
		claimed, err := scanConversationTurn(tx.QueryRowContext(ctx,
			`SELECT `+conversationTurnSelectCols+`
			   FROM conversation_turn
			  WHERE conversation_id=? AND status=?
			  ORDER BY sequence ASC LIMIT 1`, conversationID, ConversationTurnQueued))
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversation_turn SET status=?, started_at=? WHERE id=?`,
			ConversationTurnRunning, now, claimed.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE task SET status=? WHERE task_id=?`,
			ConversationTurnRunning, claimed.TaskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversation
			   SET state=?, active_turn_id=?, last_error=NULL, updated_at=?
			 WHERE id=?`, ConversationStateRunning, claimed.ID, now, conversation.ID); err != nil {
			return err
		}
		if _, err := appendConversationEvent(ctx, tx, conversation.ID, claimed.ID,
			"turn.started", map[string]any{"turn_id": claimed.ID, "task_id": claimed.TaskID}); err != nil {
			return err
		}
		claimed.Status = ConversationTurnRunning
		claimed.StartedAt = sql.NullInt64{Int64: now, Valid: true}
		turn = claimed
		return tx.Commit()
	})
	return turn, err
}

func (s *Store) GetConversationTurn(ctx context.Context, id string) (*ConversationTurn, error) {
	return scanConversationTurn(s.db.QueryRowContext(ctx,
		`SELECT `+conversationTurnSelectCols+` FROM conversation_turn WHERE id=?`, id))
}

// ConversationHasQueuedTurn reports whether an active conversation has a
// queued turn. It closes the worker handoff race between an empty queue check
// and a concurrent message submission.
func (s *Store) ConversationHasQueuedTurn(ctx context.Context, conversationID string) (bool, error) {
	var queued int
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM conversation_turn
			 WHERE conversation_id=? AND status=?
		)`, conversationID, ConversationTurnQueued).Scan(&queued)
	return queued != 0, err
}

// ListActiveConversationTurns returns every active hosted turn that needs an
// explicit recovery decision after the control plane restarts.
func (s *Store) ListActiveConversationTurns(ctx context.Context) ([]*ConversationTurn, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.conversation_id, t.task_id, t.sequence, t.prompt, t.mode,
		       t.model, t.reasoning_effort, t.context_tier, t.status,
		       t.error_message, t.created_at, t.started_at, t.finished_at
		  FROM conversation_turn t
		  JOIN conversation c ON c.active_turn_id=t.id
		 WHERE c.archived_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var turns []*ConversationTurn
	for rows.Next() {
		turn, err := scanConversationTurn(rows)
		if err != nil {
			return nil, err
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}

// GetConversationInteraction reads one interaction, including its private
// provider request ID for coordinator-only callback delivery.
func (s *Store) GetConversationInteraction(ctx context.Context, id string) (*ConversationInteraction, error) {
	return scanConversationInteraction(s.db.QueryRowContext(ctx,
		`SELECT `+conversationInteractionSelectCols+`
		   FROM conversation_interaction WHERE id=?`, id))
}

// GetConversationInteractionByProviderRequest returns an interaction already
// persisted for a native SDK request. It supports a callback reinstalled while
// a session is being recovered without creating a second public card.
func (s *Store) GetConversationInteractionByProviderRequest(ctx context.Context, conversationID, providerRequestID string) (*ConversationInteraction, error) {
	return scanConversationInteraction(s.db.QueryRowContext(ctx,
		`SELECT `+conversationInteractionSelectCols+`
		   FROM conversation_interaction
		  WHERE conversation_id=? AND provider_request_id=?`,
		conversationID, providerRequestID))
}

// CreateConversationInteraction persists a native SDK request before the
// caller blocks in the direct callback, making console reconnects safe.
func (s *Store) CreateConversationInteraction(ctx context.Context, interaction *ConversationInteraction) error {
	if interaction == nil || interaction.ID == "" || interaction.ConversationID == "" ||
		interaction.TurnID == "" || interaction.ProviderRequestID == "" {
		return errors.New("invalid conversation interaction")
	}
	if interaction.Type != ConversationInteractionInput && interaction.Type != ConversationInteractionPlan {
		return errors.New("invalid conversation interaction type")
	}
	choices, err := json.Marshal(interaction.Choices)
	if err != nil {
		return err
	}
	actions, err := json.Marshal(interaction.Actions)
	if err != nil {
		return err
	}
	return s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		conversation, err := scanConversation(tx.QueryRowContext(ctx,
			`SELECT `+conversationSelectCols+` FROM conversation WHERE id=?`, interaction.ConversationID))
		if err != nil {
			return err
		}
		if !conversation.ActiveTurnID.Valid || conversation.ActiveTurnID.String != interaction.TurnID {
			return ErrConflict
		}
		sequence, err := nextConversationSequence(ctx, tx, interaction.ConversationID)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversation_interaction
			    (id, conversation_id, turn_id, sequence, type, status,
			     provider_request_id, question, choices_json, allow_freeform,
			     summary, plan, actions_json, recommended_action, answer,
			     approved, selected_action, feedback, created_at, resolved_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL, NULL, ?, NULL)`,
			interaction.ID, interaction.ConversationID, interaction.TurnID, sequence,
			interaction.Type, ConversationInteractionPending,
			interaction.ProviderRequestID, interaction.Question, string(choices),
			boolToInt(interaction.AllowFreeform), interaction.Summary, interaction.Plan,
			string(actions), interaction.RecommendedAction, now); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		state, turnState := ConversationStateWaitingInput, ConversationTurnWaitingInput
		if interaction.Type == ConversationInteractionPlan {
			state, turnState = ConversationStateWaitingPlan, ConversationTurnWaitingPlan
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE conversation_turn SET status=? WHERE id=?`, turnState, interaction.TurnID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE task SET status=? WHERE task_id=(SELECT task_id FROM conversation_turn WHERE id=?)`,
			turnState, interaction.TurnID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversation SET state=?, updated_at=? WHERE id=?`,
			state, now, interaction.ConversationID); err != nil {
			return err
		}
		interaction.Sequence = sequence
		interaction.Status = ConversationInteractionPending
		interaction.CreatedAt = time.Unix(now, 0).UTC()
		if _, err := appendConversationEvent(ctx, tx, interaction.ConversationID, interaction.TurnID,
			"interaction.requested", publicInteractionPayload(interaction)); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// ResolveConversationInteraction performs the durable compare-and-set before a
// coordinator signals the SDK callback. A duplicate click can never submit two
// answers to the provider.
func (s *Store) ResolveConversationInteraction(ctx context.Context, id, answer string, approved *bool, selectedAction, feedback string) (*ConversationInteraction, error) {
	var resolved *ConversationInteraction
	err := s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		interaction, err := scanConversationInteraction(tx.QueryRowContext(ctx,
			`SELECT `+conversationInteractionSelectCols+`
			   FROM conversation_interaction WHERE id=?`, id))
		if err != nil {
			return err
		}
		if interaction.Status != ConversationInteractionPending {
			return ErrConflict
		}
		now := time.Now().Unix()
		var approvedValue any
		if approved != nil {
			approvedValue = boolToInt(*approved)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversation_interaction
			   SET status=?, answer=?, approved=?, selected_action=?, feedback=?, resolved_at=?
			 WHERE id=? AND status=?`,
			ConversationInteractionResolved, nullIfEmpty(answer), approvedValue,
			nullIfEmpty(selectedAction), nullIfEmpty(feedback), now, interaction.ID,
			ConversationInteractionPending); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE conversation_turn SET status=? WHERE id=?`,
			ConversationTurnRunning, interaction.TurnID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE task SET status=?
			 WHERE task_id=(SELECT task_id FROM conversation_turn WHERE id=?)`,
			ConversationTurnRunning, interaction.TurnID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversation
			   SET state=?, last_error=NULL, updated_at=?
			 WHERE id=?`, ConversationStateRunning, now, interaction.ConversationID); err != nil {
			return err
		}
		interaction.Status = ConversationInteractionResolved
		interaction.Answer = nullIfEmpty(answer)
		if approved != nil {
			interaction.Approved = sql.NullBool{Bool: *approved, Valid: true}
		}
		interaction.SelectedAction = nullIfEmpty(selectedAction)
		interaction.Feedback = nullIfEmpty(feedback)
		interaction.ResolvedAt = sql.NullInt64{Int64: now, Valid: true}
		resolved = interaction
		_, err = appendConversationEvent(ctx, tx, interaction.ConversationID, interaction.TurnID,
			"interaction.resolved", map[string]any{
				"id": interaction.ID, "type": interaction.Type, "status": ConversationInteractionResolved,
			})
		if err != nil {
			return err
		}
		return tx.Commit()
	})
	return resolved, err
}

// AppendAssistantText appends one sanitized model delta and emits a narrow
// replay event. The transcript is capped to keep a malformed provider stream
// from filling the control-plane database.
func (s *Store) AppendAssistantText(ctx context.Context, conversationID, turnID, messageID, text string) error {
	if text == "" {
		return nil
	}
	if len(text) > maxConversationEventPayload {
		return errors.New("conversation message delta exceeds limit")
	}
	return s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var existing int
		err = tx.QueryRowContext(ctx,
			`SELECT length(content) FROM conversation_message WHERE id=?`, messageID).Scan(&existing)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			sequence, err := nextConversationSequence(ctx, tx, conversationID)
			if err != nil {
				return err
			}
			now := time.Now().Unix()
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO conversation_message
				    (id, conversation_id, turn_id, sequence, role, content, status, created_at)
				VALUES (?, ?, ?, ?, 'assistant', ?, ?, ?)`,
				messageID, conversationID, turnID, sequence, text,
				conversationMessageStreaming, now); err != nil {
				return err
			}
		case err != nil:
			return err
		case existing+len(text) > maxConversationMessageBytes:
			return errors.New("conversation message exceeds limit")
		default:
			if _, err := tx.ExecContext(ctx, `
				UPDATE conversation_message SET content=content || ?, status=?
				 WHERE id=?`, text, conversationMessageStreaming, messageID); err != nil {
				return err
			}
		}
		if _, err := appendConversationEvent(ctx, tx, conversationID, turnID,
			"message.delta", map[string]any{"id": messageID, "turn_id": turnID, "text": text}); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// AppendConversationEvent records an already-redacted provider lifecycle event.
func (s *Store) AppendConversationEvent(ctx context.Context, conversationID, turnID, typ string, payload any) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := appendConversationEvent(ctx, db, conversationID, turnID, typ, payload)
		return err
	})
}

// FinishConversationTurn stores the canonical hosted result in the existing
// task row, releases the conversation queue, and interrupts any stale callback.
func (s *Store) FinishConversationTurn(ctx context.Context, turnID, status, resultJSON, errorMessage string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		turn, err := scanConversationTurn(tx.QueryRowContext(ctx,
			`SELECT `+conversationTurnSelectCols+` FROM conversation_turn WHERE id=?`, turnID))
		if err != nil {
			return err
		}
		conversation, err := scanConversation(tx.QueryRowContext(ctx,
			`SELECT `+conversationSelectCols+` FROM conversation WHERE id=?`, turn.ConversationID))
		if err != nil {
			return err
		}
		// A cancellation/disconnect recovery may have already finalized this
		// turn while the provider goroutine was unwinding. Never let that stale
		// goroutine overwrite the durable terminal result or release a newer
		// turn's queue ownership.
		if !conversation.ActiveTurnID.Valid || conversation.ActiveTurnID.String != turn.ID {
			return ErrConflict
		}
		now := time.Now().Unix()
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversation_interaction
			   SET status=?, resolved_at=?
			 WHERE turn_id=? AND status=?`,
			ConversationInteractionInterrupted, now, turn.ID, ConversationInteractionPending); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversation_message SET status=?
			 WHERE turn_id=? AND role='assistant' AND status=?`,
			conversationMessageComplete, turn.ID, conversationMessageStreaming); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversation_turn
			   SET status=?, error_message=?, finished_at=?
			 WHERE id=?`, status, nullIfEmpty(errorMessage), now, turn.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE task SET status=?, result_json=?, finished_at=?
			 WHERE task_id=?`, status, nullIfEmpty(resultJSON), now, turn.TaskID); err != nil {
			return err
		}
		conversationState := ConversationStateIdle
		if status == ConversationTurnFailed {
			conversationState = ConversationStateFailed
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversation
			   SET state=?, active_turn_id=NULL, last_error=?, updated_at=?
			 WHERE id=?`, conversationState, nullIfEmpty(errorMessage), now, turn.ConversationID); err != nil {
			return err
		}
		if _, err := appendConversationEvent(ctx, tx, turn.ConversationID, turn.ID,
			"turn.completed", map[string]any{
				"turn_id": turn.ID, "task_id": turn.TaskID, "status": status,
				"error_message": errorMessage,
			}); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// RequestConversationTurnCancellation transitions an active turn to a state
// that still protects the sandbox until the SDK worker finalizes it.
func (s *Store) RequestConversationTurnCancellation(ctx context.Context, conversationID string) (*ConversationTurn, error) {
	var current *ConversationTurn
	err := s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		conversation, err := scanConversation(tx.QueryRowContext(ctx,
			`SELECT `+conversationSelectCols+` FROM conversation WHERE id=?`, conversationID))
		if err != nil {
			return err
		}
		if !conversation.ActiveTurnID.Valid {
			return ErrNotFound
		}
		turn, err := scanConversationTurn(tx.QueryRowContext(ctx,
			`SELECT `+conversationTurnSelectCols+` FROM conversation_turn WHERE id=?`,
			conversation.ActiveTurnID.String))
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversation_interaction
			   SET status=?, resolved_at=?
			 WHERE turn_id=? AND status=?`,
			ConversationInteractionInterrupted, now, turn.ID, ConversationInteractionPending); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE conversation_turn SET status=? WHERE id=?`,
			ConversationTurnCancelling, turn.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE task SET status=? WHERE task_id=?`,
			ConversationTurnRunning, turn.TaskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversation SET state=?, updated_at=? WHERE id=?`,
			ConversationStateRunning, now, conversation.ID); err != nil {
			return err
		}
		if _, err := appendConversationEvent(ctx, tx, conversation.ID, turn.ID,
			"turn.cancelling", map[string]any{"turn_id": turn.ID}); err != nil {
			return err
		}
		turn.Status = ConversationTurnCancelling
		current = turn
		return tx.Commit()
	})
	return current, err
}

// InterruptConversationTurn makes a pending/active turn visibly terminal when
// its SDK session cannot be resumed, such as after provider disconnect.
func (s *Store) InterruptConversationTurn(ctx context.Context, turnID, message string) error {
	turn, err := s.GetConversationTurn(ctx, turnID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	startedAt := turn.CreatedAt
	if turn.StartedAt.Valid {
		startedAt = time.Unix(turn.StartedAt.Int64, 0).UTC()
	}
	result, err := json.Marshal(runtime.TaskResult{
		ID:            turn.TaskID,
		Prompt:        turn.Prompt,
		Status:        runtime.TaskFailed,
		FailureReason: "provider_interrupted",
		ErrorMessage:  message,
		FilesChanged:  []string{},
		BuildStatus:   runtime.BuildSkipped,
		CreatedAt:     turn.CreatedAt,
		StartedAt:     startedAt,
		FinishedAt:    now,
		DurationMS:    now.Sub(startedAt).Milliseconds(),
	})
	if err != nil {
		return err
	}
	return s.FinishConversationTurn(ctx, turnID, ConversationTurnFailed, string(result), message)
}

// SnapshotActiveConversation reads the complete current UI state. The bounded
// transcript is returned oldest-first for direct rendering.
func (s *Store) SnapshotActiveConversation(ctx context.Context, sandboxID string) (*ConversationSnapshot, error) {
	// The cursor must describe the same SQLite snapshot as the transcript. If it
	// were read separately, a client could skip an event committed between the
	// transcript reads and the cursor query during its SSE handoff.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	conversation, err := getActiveConversation(ctx, tx, sandboxID)
	if err != nil {
		return nil, err
	}
	turns, err := listConversationTurns(ctx, tx, conversation.ID, 200)
	if err != nil {
		return nil, err
	}
	messages, err := listConversationMessages(ctx, tx, conversation.ID, 500)
	if err != nil {
		return nil, err
	}
	interactions, err := listConversationInteractions(ctx, tx, conversation.ID, 200)
	if err != nil {
		return nil, err
	}
	children, err := listConversationChildren(ctx, tx, conversation.ID, 100)
	if err != nil {
		return nil, err
	}
	cursor, err := latestConversationEventID(ctx, tx, conversation.ID)
	if err != nil {
		return nil, err
	}
	next := 0
	for _, turn := range turns {
		if turn.Status == ConversationTurnQueued {
			next++
		}
	}
	snapshot := &ConversationSnapshot{
		Conversation: conversation, Turns: turns, Messages: messages,
		Interactions: interactions, Children: children,
		EventCursor: cursor, NextQueueSlot: next,
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *Store) listConversationTurns(ctx context.Context, conversationID string, limit int) ([]*ConversationTurn, error) {
	return listConversationTurns(ctx, s.db, conversationID, limit)
}

func listConversationTurns(ctx context.Context, queryer conversationQueryer, conversationID string, limit int) ([]*ConversationTurn, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := queryer.QueryContext(ctx, `
		SELECT `+conversationTurnSelectCols+`
		  FROM (
			SELECT `+conversationTurnSelectCols+`
			  FROM conversation_turn WHERE conversation_id=?
			  ORDER BY sequence DESC LIMIT ?
		  )
		 ORDER BY sequence ASC`, conversationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := []*ConversationTurn{}
	for rows.Next() {
		turn, err := scanConversationTurn(rows)
		if err != nil {
			return nil, err
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}

func (s *Store) listConversationMessages(ctx context.Context, conversationID string, limit int) ([]*ConversationMessage, error) {
	return listConversationMessages(ctx, s.db, conversationID, limit)
}

func listConversationMessages(ctx context.Context, queryer conversationQueryer, conversationID string, limit int) ([]*ConversationMessage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := queryer.QueryContext(ctx, `
		SELECT `+conversationMessageSelectCols+`
		  FROM (
			SELECT `+conversationMessageSelectCols+`
			  FROM conversation_message WHERE conversation_id=?
			  ORDER BY sequence DESC LIMIT ?
		  )
		 ORDER BY sequence ASC`, conversationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := []*ConversationMessage{}
	for rows.Next() {
		message, err := scanConversationMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *Store) listConversationInteractions(ctx context.Context, conversationID string, limit int) ([]*ConversationInteraction, error) {
	return listConversationInteractions(ctx, s.db, conversationID, limit)
}

func listConversationInteractions(ctx context.Context, queryer conversationQueryer, conversationID string, limit int) ([]*ConversationInteraction, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := queryer.QueryContext(ctx, `
		SELECT `+conversationInteractionSelectCols+`
		  FROM (
			SELECT `+conversationInteractionSelectCols+`
			  FROM conversation_interaction WHERE conversation_id=?
			  ORDER BY sequence DESC LIMIT ?
		  )
		 ORDER BY sequence ASC`, conversationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	interactions := []*ConversationInteraction{}
	for rows.Next() {
		interaction, err := scanConversationInteraction(rows)
		if err != nil {
			return nil, err
		}
		interactions = append(interactions, interaction)
	}
	return interactions, rows.Err()
}

// ListConversationEvents returns a bounded ordered event page after the given
// cursor. EventSource reconnection uses this to replay missed updates.
func (s *Store) ListConversationEvents(ctx context.Context, conversationID string, after int64, limit int) ([]*ConversationEvent, error) {
	if after < 0 {
		after = 0
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, conversation_id, turn_id, type, payload_json, created_at
		  FROM conversation_event
		 WHERE conversation_id=? AND id>?
		 ORDER BY id ASC LIMIT ?`, conversationID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []*ConversationEvent{}
	for rows.Next() {
		event, err := scanConversationEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) LatestConversationEventID(ctx context.Context, conversationID string) (int64, error) {
	return latestConversationEventID(ctx, s.db, conversationID)
}

func latestConversationEventID(ctx context.Context, queryer conversationQueryer, conversationID string) (int64, error) {
	var id sql.NullInt64
	err := queryer.QueryRowContext(ctx,
		`SELECT MAX(id) FROM conversation_event WHERE conversation_id=?`, conversationID).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id.Int64, nil
}

func nextConversationSequence(ctx context.Context, tx *sql.Tx, conversationID string) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversation SET next_sequence=next_sequence+1 WHERE id=?`, conversationID); err != nil {
		return 0, err
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx,
		`SELECT next_sequence FROM conversation WHERE id=?`, conversationID).Scan(&sequence); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return sequence, nil
}

func appendConversationEvent(ctx context.Context, exec sqlExecutor, conversationID, turnID, typ string, payload any) (int64, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	if len(raw) > maxConversationEventPayload {
		return 0, errors.New("conversation event exceeds limit")
	}
	result, err := exec.ExecContext(ctx, `
		INSERT INTO conversation_event
		    (conversation_id, turn_id, type, payload_json, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		conversationID, nullIfEmpty(turnID), typ, string(raw), time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func publicInteractionPayload(interaction *ConversationInteraction) map[string]any {
	return map[string]any{
		"id": interaction.ID, "turn_id": interaction.TurnID, "sequence": interaction.Sequence,
		"type": interaction.Type, "status": interaction.Status, "question": interaction.Question,
		"choices": interaction.Choices, "allow_freeform": interaction.AllowFreeform,
		"summary": interaction.Summary, "plan": interaction.Plan, "actions": interaction.Actions,
		"recommended_action": interaction.RecommendedAction,
	}
}
