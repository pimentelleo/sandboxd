package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	ConversationChildQueued      = "queued"
	ConversationChildPreparing   = "preparing"
	ConversationChildRunning     = "running"
	ConversationChildCancelling  = "cancelling"
	ConversationChildSucceeded   = "succeeded"
	ConversationChildFailed      = "failed"
	ConversationChildCancelled   = "cancelled"
	ConversationChildInterrupted = "interrupted"

	ConversationChildPatchNone        = "none"
	ConversationChildPatchAvailable   = "available"
	ConversationChildPatchUnavailable = "unavailable"

	maxConversationChildPrompt = 64 << 10
	maxConversationChildLabel  = 256
	maxConversationChildResult = 64 << 10

	// MaxConversationChildPatchBytes caps serialized review data retained for
	// an isolated worker.
	MaxConversationChildPatchBytes = 512 << 10
	// MaxConversationChildPatchFiles bounds both patch capture and client
	// review fan-out.
	MaxConversationChildPatchFiles = 100

	// MaxConversationChildPatchFileBytes keeps a single read_delegated_change
	// response safely below the regular Copilot tool output limit.
	MaxConversationChildPatchFileBytes = 48 << 10
)

// ConversationChild is an isolated background agent delegated by one parent
// turn. WorkspacePath, WorkerContainer, and PatchJSON are control-plane-only
// fields and must never be returned directly to an API client.
type ConversationChild struct {
	ID              string
	ConversationID  string
	ParentTurnID    string
	Label           string
	Prompt          string
	Model           string
	ReasoningEffort string
	ContextTier     string
	Status          string
	WorkspacePath   string `json:"-"`
	WorkerContainer string `json:"-"`
	Result          string
	ErrorMessage    sql.NullString
	PatchState      string
	PatchJSON       string `json:"-"`
	CreatedAt       time.Time
	StartedAt       sql.NullInt64
	FinishedAt      sql.NullInt64
}

// ConversationChildPatch is a bounded, structured patch returned by an
// isolated worker. The parent harness reviews its file content before using
// its ordinary workspace tools to apply any change.
type ConversationChildPatch struct {
	Changes []ConversationChildChange `json:"changes"`
}

type ConversationChildChange struct {
	Path       string `json:"path"`
	BaseSHA256 string `json:"base_sha256,omitempty"`
	Content    string `json:"content,omitempty"`
	Deleted    bool   `json:"deleted,omitempty"`
	Mode       uint32 `json:"mode,omitempty"`
}

const conversationChildSelectCols = `id, conversation_id, parent_turn_id,
	label, prompt, model, reasoning_effort, context_tier, status, workspace_path,
	worker_container, result, error_message, patch_state, patch_json, created_at,
	started_at, finished_at`

func scanConversationChild(sc scanner) (*ConversationChild, error) {
	var child ConversationChild
	var created int64
	if err := sc.Scan(&child.ID, &child.ConversationID, &child.ParentTurnID,
		&child.Label, &child.Prompt, &child.Model, &child.ReasoningEffort,
		&child.ContextTier, &child.Status, &child.WorkspacePath,
		&child.WorkerContainer, &child.Result, &child.ErrorMessage,
		&child.PatchState, &child.PatchJSON, &created, &child.StartedAt,
		&child.FinishedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	child.CreatedAt = time.Unix(created, 0).UTC()
	return &child, nil
}

// CreateConversationChild persists a child only while its parent turn is the
// conversation's active turn. Model settings are copied from that durable turn
// rather than trusted from a tool request.
func (s *Store) CreateConversationChild(ctx context.Context, child *ConversationChild) error {
	if child == nil || child.ID == "" || child.ConversationID == "" || child.ParentTurnID == "" ||
		child.WorkspacePath == "" || child.Prompt == "" || len(child.Prompt) > maxConversationChildPrompt ||
		len(child.Label) > maxConversationChildLabel {
		return errors.New("invalid conversation child")
	}
	if child.Status == "" {
		child.Status = ConversationChildQueued
	}
	if child.Status != ConversationChildQueued {
		return errors.New("invalid conversation child status")
	}
	return s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		conversation, err := scanConversation(tx.QueryRowContext(ctx,
			`SELECT `+conversationSelectCols+` FROM conversation WHERE id=?`, child.ConversationID))
		if err != nil {
			return err
		}
		if conversation.ArchivedAt.Valid || !conversation.ActiveTurnID.Valid ||
			conversation.ActiveTurnID.String != child.ParentTurnID {
			return ErrConflict
		}
		turn, err := scanConversationTurn(tx.QueryRowContext(ctx,
			`SELECT `+conversationTurnSelectCols+` FROM conversation_turn WHERE id=?`, child.ParentTurnID))
		if err != nil {
			return err
		}
		if turn.ConversationID != child.ConversationID {
			return ErrConflict
		}

		now := time.Now().Unix()
		child.Model = turn.Model
		child.ReasoningEffort = turn.ReasoningEffort
		child.ContextTier = turn.ContextTier
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversation_child
			    (id, conversation_id, parent_turn_id, label, prompt, model,
			     reasoning_effort, context_tier, status, workspace_path,
			     worker_container, result, error_message, patch_state, patch_json,
			     created_at, started_at, finished_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', NULL, ?, '', ?, NULL, NULL)`,
			child.ID, child.ConversationID, child.ParentTurnID, child.Label, child.Prompt,
			child.Model, child.ReasoningEffort, child.ContextTier, child.Status,
			child.WorkspacePath, ConversationChildPatchNone, now); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		child.PatchState = ConversationChildPatchNone
		child.CreatedAt = time.Unix(now, 0).UTC()
		if _, err := appendConversationEvent(ctx, tx, child.ConversationID, child.ParentTurnID,
			"child.created", publicConversationChildPayload(child)); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// ClaimConversationChild starts one queued child. A cancellation that wins the
// race is left terminally cancelling for the worker to clean up.
func (s *Store) ClaimConversationChild(ctx context.Context, id string) (*ConversationChild, error) {
	var claimed *ConversationChild
	err := s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		child, err := scanConversationChild(tx.QueryRowContext(ctx,
			`SELECT `+conversationChildSelectCols+` FROM conversation_child WHERE id=?`, id))
		if err != nil {
			return err
		}
		if child.Status != ConversationChildQueued {
			return ErrConflict
		}
		now := time.Now().Unix()
		result, err := tx.ExecContext(ctx, `
			UPDATE conversation_child SET status=?, started_at=? WHERE id=? AND status=?`,
			ConversationChildPreparing, now, child.ID, ConversationChildQueued)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrConflict
		}
		child.Status = ConversationChildPreparing
		child.StartedAt = sql.NullInt64{Int64: now, Valid: true}
		if _, err := appendConversationEvent(ctx, tx, child.ConversationID, child.ParentTurnID,
			"child.updated", publicConversationChildPayload(child)); err != nil {
			return err
		}
		claimed = child
		return tx.Commit()
	})
	return claimed, err
}

// StartConversationChild records the only container allowed to service this
// child before its separate SDK session begins.
func (s *Store) StartConversationChild(ctx context.Context, id, workerContainer string) (*ConversationChild, error) {
	if workerContainer == "" {
		return nil, errors.New("invalid worker container")
	}
	var started *ConversationChild
	err := s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		child, err := scanConversationChild(tx.QueryRowContext(ctx,
			`SELECT `+conversationChildSelectCols+` FROM conversation_child WHERE id=?`, id))
		if err != nil {
			return err
		}
		if child.Status != ConversationChildPreparing {
			return ErrConflict
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE conversation_child SET status=?, worker_container=?
			 WHERE id=? AND status=?`,
			ConversationChildRunning, workerContainer, id, ConversationChildPreparing)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrConflict
		}
		child.Status = ConversationChildRunning
		child.WorkerContainer = workerContainer
		if _, err := appendConversationEvent(ctx, tx, child.ConversationID, child.ParentTurnID,
			"child.updated", publicConversationChildPayload(child)); err != nil {
			return err
		}
		started = child
		return tx.Commit()
	})
	return started, err
}

// FinishConversationChild stores a bounded result and optional structured
// patch. A durable cancellation always wins over a late worker completion.
func (s *Store) FinishConversationChild(ctx context.Context, id, status, resultText, errorMessage, patchState string, patch *ConversationChildPatch) (*ConversationChild, error) {
	if !terminalConversationChildStatus(status) || len(resultText) > maxConversationChildResult ||
		!validConversationChildPatchState(patchState) {
		return nil, errors.New("invalid conversation child result")
	}
	patchJSON := ""
	if patchState == ConversationChildPatchAvailable {
		if patch == nil {
			return nil, errors.New("missing conversation child patch")
		}
		if err := validateConversationChildPatch(patch); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(patch)
		if err != nil {
			return nil, err
		}
		if len(raw) > MaxConversationChildPatchBytes {
			return nil, errors.New("conversation child patch exceeds limit")
		}
		patchJSON = string(raw)
	} else if patch != nil {
		return nil, errors.New("unexpected conversation child patch")
	}

	var finished *ConversationChild
	err := s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		child, err := scanConversationChild(tx.QueryRowContext(ctx,
			`SELECT `+conversationChildSelectCols+` FROM conversation_child WHERE id=?`, id))
		if err != nil {
			return err
		}
		if terminalConversationChildStatus(child.Status) {
			return ErrConflict
		}
		if child.Status != ConversationChildPreparing && child.Status != ConversationChildRunning &&
			child.Status != ConversationChildCancelling {
			return ErrConflict
		}
		if child.Status == ConversationChildCancelling {
			status, resultText, errorMessage, patchState, patchJSON = ConversationChildCancelled, "", "",
				ConversationChildPatchNone, ""
		}
		now := time.Now().Unix()
		result, err := tx.ExecContext(ctx, `
			UPDATE conversation_child
			   SET status=?, result=?, error_message=?, patch_state=?, patch_json=?, finished_at=?
			 WHERE id=? AND status IN (?, ?, ?)`,
			status, resultText, nullIfEmpty(errorMessage), patchState, patchJSON, now, child.ID,
			ConversationChildPreparing, ConversationChildRunning, ConversationChildCancelling)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrConflict
		}
		child.Status = status
		child.Result = resultText
		child.ErrorMessage = nullIfEmpty(errorMessage)
		child.PatchState = patchState
		child.PatchJSON = patchJSON
		child.FinishedAt = sql.NullInt64{Int64: now, Valid: true}
		if _, err := appendConversationEvent(ctx, tx, child.ConversationID, child.ParentTurnID,
			"child.updated", publicConversationChildPayload(child)); err != nil {
			return err
		}
		finished = child
		return tx.Commit()
	})
	return finished, err
}

// RequestConversationChildCancellation makes cancellation durable before the
// in-memory SDK cancellation is attempted.
func (s *Store) RequestConversationChildCancellation(ctx context.Context, conversationID, id string) (*ConversationChild, error) {
	var cancelled *ConversationChild
	err := s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		child, err := scanConversationChild(tx.QueryRowContext(ctx,
			`SELECT `+conversationChildSelectCols+`
			   FROM conversation_child WHERE id=? AND conversation_id=?`, id, conversationID))
		if err != nil {
			return err
		}
		if terminalConversationChildStatus(child.Status) || child.Status == ConversationChildCancelling {
			return ErrConflict
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE conversation_child SET status=?
			 WHERE id=? AND status IN (?, ?, ?)`,
			ConversationChildCancelling, child.ID, ConversationChildQueued,
			ConversationChildPreparing, ConversationChildRunning)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrConflict
		}
		child.Status = ConversationChildCancelling
		if _, err := appendConversationEvent(ctx, tx, child.ConversationID, child.ParentTurnID,
			"child.updated", publicConversationChildPayload(child)); err != nil {
			return err
		}
		cancelled = child
		return tx.Commit()
	})
	return cancelled, err
}

func (s *Store) GetConversationChild(ctx context.Context, id string) (*ConversationChild, error) {
	return scanConversationChild(s.db.QueryRowContext(ctx,
		`SELECT `+conversationChildSelectCols+` FROM conversation_child WHERE id=?`, id))
}

func (s *Store) GetConversationChildForConversation(ctx context.Context, conversationID, id string) (*ConversationChild, error) {
	return scanConversationChild(s.db.QueryRowContext(ctx,
		`SELECT `+conversationChildSelectCols+`
		   FROM conversation_child WHERE id=? AND conversation_id=?`, id, conversationID))
}

func (s *Store) ListConversationChildren(ctx context.Context, conversationID string, limit int) ([]*ConversationChild, error) {
	return listConversationChildren(ctx, s.db, conversationID, limit)
}

// CountActiveConversationChildren includes queued work so callers can bound
// admission before an unbounded number of SDK workers is created.
func (s *Store) CountActiveConversationChildren(ctx context.Context, conversationID string) (int, error) {
	return countActiveConversationChildren(ctx, s.db, `conversation_id=?`, []any{conversationID})
}

// CountAllActiveConversationChildren is the global counterpart used for the
// control-plane-wide delegation cap.
func (s *Store) CountAllActiveConversationChildren(ctx context.Context) (int, error) {
	return countActiveConversationChildren(ctx, s.db, "1=1", nil)
}

func countActiveConversationChildren(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, where string, args []any) (int, error) {
	query := `SELECT COUNT(*) FROM conversation_child WHERE ` + where +
		` AND status IN (?, ?, ?, ?)`
	args = append(append([]any(nil), args...), ConversationChildQueued,
		ConversationChildPreparing, ConversationChildRunning, ConversationChildCancelling)
	var count int
	if err := queryer.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func listConversationChildren(ctx context.Context, queryer conversationQueryer, conversationID string, limit int) ([]*ConversationChild, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := queryer.QueryContext(ctx, `
		SELECT `+conversationChildSelectCols+`
		  FROM conversation_child
		 WHERE conversation_id=?
		 ORDER BY created_at ASC, id ASC LIMIT ?`, conversationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	children := []*ConversationChild{}
	for rows.Next() {
		child, err := scanConversationChild(rows)
		if err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return children, rows.Err()
}

// InterruptActiveConversationChildren makes work that lost its in-memory SDK
// owner explicit after a restart or credential invalidation.
func (s *Store) InterruptActiveConversationChildren(ctx context.Context, conversationID, message string) ([]*ConversationChild, error) {
	return s.interruptConversationChildren(ctx, `conversation_id=?`, []any{conversationID}, message)
}

// InterruptAllActiveConversationChildren is the recovery/credential-rotation
// variant across every conversation.
func (s *Store) InterruptAllActiveConversationChildren(ctx context.Context, message string) ([]*ConversationChild, error) {
	return s.interruptConversationChildren(ctx, "1=1", nil, message)
}

func (s *Store) interruptConversationChildren(ctx context.Context, where string, args []any, message string) ([]*ConversationChild, error) {
	var interrupted []*ConversationChild
	err := s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		query := `SELECT ` + conversationChildSelectCols + `
			FROM conversation_child WHERE ` + where + `
			  AND status IN (?, ?, ?, ?)`
		queryArgs := append(append([]any(nil), args...), ConversationChildQueued,
			ConversationChildPreparing, ConversationChildRunning, ConversationChildCancelling)
		rows, err := tx.QueryContext(ctx, query, queryArgs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			child, err := scanConversationChild(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			interrupted = append(interrupted, child)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		now := time.Now().Unix()
		for _, child := range interrupted {
			if _, err := tx.ExecContext(ctx, `
				UPDATE conversation_child
				   SET status=?, error_message=?, patch_state=?, patch_json='', finished_at=?
				 WHERE id=?`,
				ConversationChildInterrupted, nullIfEmpty(message),
				ConversationChildPatchNone, now, child.ID); err != nil {
				return err
			}
			child.Status = ConversationChildInterrupted
			child.ErrorMessage = nullIfEmpty(message)
			child.PatchState = ConversationChildPatchNone
			child.PatchJSON = ""
			child.FinishedAt = sql.NullInt64{Int64: now, Valid: true}
			if _, err := appendConversationEvent(ctx, tx, child.ConversationID, child.ParentTurnID,
				"child.updated", publicConversationChildPayload(child)); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
	return interrupted, err
}

// Patch returns the parsed patch only when a bounded patch is available.
func (child *ConversationChild) Patch() (*ConversationChildPatch, error) {
	if child == nil || child.PatchState != ConversationChildPatchAvailable || child.PatchJSON == "" {
		return nil, ErrNotFound
	}
	var patch ConversationChildPatch
	if err := json.Unmarshal([]byte(child.PatchJSON), &patch); err != nil {
		return nil, errors.New("invalid stored conversation child patch")
	}
	if err := validateConversationChildPatch(&patch); err != nil {
		return nil, errors.New("invalid stored conversation child patch")
	}
	return &patch, nil
}

func (child *ConversationChild) ChangedFiles() []string {
	patch, err := child.Patch()
	if err != nil {
		return []string{}
	}
	files := make([]string, 0, len(patch.Changes))
	for _, change := range patch.Changes {
		files = append(files, change.Path)
	}
	return files
}

func terminalConversationChildStatus(status string) bool {
	switch status {
	case ConversationChildSucceeded, ConversationChildFailed, ConversationChildCancelled, ConversationChildInterrupted:
		return true
	default:
		return false
	}
}

func validConversationChildPatchState(state string) bool {
	switch state {
	case ConversationChildPatchNone, ConversationChildPatchAvailable, ConversationChildPatchUnavailable:
		return true
	default:
		return false
	}
}

func validateConversationChildPatch(patch *ConversationChildPatch) error {
	if patch == nil || len(patch.Changes) == 0 || len(patch.Changes) > MaxConversationChildPatchFiles {
		return errors.New("invalid conversation child patch")
	}
	seen := make(map[string]bool, len(patch.Changes))
	total := 0
	for _, change := range patch.Changes {
		if !validConversationChildPath(change.Path) || seen[change.Path] ||
			len(change.BaseSHA256) > 128 || len(change.Content) > MaxConversationChildPatchFileBytes ||
			(change.Deleted && change.Content != "") {
			return errors.New("invalid conversation child patch")
		}
		seen[change.Path] = true
		total += len(change.Path) + len(change.BaseSHA256) + len(change.Content)
		if total > MaxConversationChildPatchBytes {
			return errors.New("conversation child patch exceeds limit")
		}
	}
	return nil
}

func validConversationChildPath(value string) bool {
	if value == "" || len(value) > 1024 || strings.ContainsRune(value, 0) ||
		strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func publicConversationChildPayload(child *ConversationChild) map[string]any {
	payload := map[string]any{
		"id": child.ID, "parent_turn_id": child.ParentTurnID, "label": child.Label,
		"prompt": child.Prompt, "model": child.Model,
		"reasoning_effort": child.ReasoningEffort, "context_tier": child.ContextTier,
		"status": child.Status, "result": child.Result, "patch_state": child.PatchState,
		"changed_files": child.ChangedFiles(),
	}
	if child.ErrorMessage.Valid {
		payload["error_message"] = child.ErrorMessage.String
	}
	return payload
}
