package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Task is the in-memory mirror of a `task` table row — sandboxd's
// durable record of one coding task (runtimed slice 5). It exists so a
// task's canonical result outlives the sandbox.
type Task struct {
	TaskID            string
	SandboxID         string
	ExternalUserID    sql.NullString
	ExternalProjectID sql.NullString
	Agent             string
	Prompt            string
	Status            string // queued | running | waiting_* | succeeded | failed | cancelled
	ResultJSON        sql.NullString
	TimeoutS          int // task timeout in seconds; 0 = runtimed default
	// ExecutionKind distinguishes runtimed's monolithic tasks from hosted
	// control-plane turns. Only runtimed tasks are reconciled by taskwatch.
	ExecutionKind      string
	ConversationID     sql.NullString
	ConversationTurnID sql.NullString
	CreatedAt          time.Time
	FinishedAt         sql.NullInt64
}

const (
	TaskExecutionRuntimed      = "runtimed"
	TaskExecutionHostedCopilot = "hosted-copilot"
)

var ErrTaskNotRunning = errors.New("task is no longer running")

const taskSelectCols = `task_id, sandbox_id, external_user_id, external_project_id,
	agent, prompt, status, result_json, timeout_s, execution_kind, conversation_id,
	conversation_turn_id, created_at, finished_at`

func scanTask(sc scanner) (*Task, error) {
	var t Task
	var created int64
	err := sc.Scan(&t.TaskID, &t.SandboxID, &t.ExternalUserID, &t.ExternalProjectID,
		&t.Agent, &t.Prompt, &t.Status, &t.ResultJSON, &t.TimeoutS, &t.ExecutionKind,
		&t.ConversationID, &t.ConversationTurnID, &created, &t.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = time.Unix(created, 0).UTC()
	return &t, nil
}

// CreateTask inserts a durable task row. Existing callers omit Status and keep
// the legacy running/runtimed defaults; hosted conversation turns may begin
// queued before their coordinator claims them.
func (s *Store) CreateTask(ctx context.Context, t *Task) error {
	return s.submit(ctx, func(db *dialectDB) error {
		status := t.Status
		if status == "" {
			status = "running"
		}
		executionKind := t.ExecutionKind
		if executionKind == "" {
			executionKind = TaskExecutionRuntimed
		}
		_, err := db.ExecContext(ctx, `
			INSERT INTO task
			  (task_id, sandbox_id, external_user_id, external_project_id,
			   agent, prompt, status, timeout_s, execution_kind, conversation_id,
			   conversation_turn_id, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			t.TaskID, t.SandboxID, t.ExternalUserID, t.ExternalProjectID,
			t.Agent, t.Prompt, status, t.TimeoutS, executionKind, t.ConversationID,
			t.ConversationTurnID, time.Now().Unix())
		return err
	})
}

// FinishTask records a task's terminal status and canonical result only while
// the task is still running. The conditional update keeps duplicate local
// watchers from overwriting the first durable terminal result.
func (s *Store) FinishTask(ctx context.Context, taskID, status, resultJSON string) error {
	return s.finishRunningTask(ctx, taskID, status, resultJSON, nil)
}

// FinishTaskWithLease records a task result only while the caller still owns
// its task-watch lease. This fences stale Kubernetes replicas after a lease
// expires and ensures that only one watcher can publish the terminal result.
func (s *Store) FinishTaskWithLease(ctx context.Context, lease OperationLease, taskID, status, resultJSON string) error {
	if lease.Resource != LeaseResourceTask || lease.ResourceID != taskID || lease.HolderID == "" || lease.Token == "" {
		return errors.New("invalid task completion lease")
	}
	return s.finishRunningTask(ctx, taskID, status, resultJSON, &lease)
}

func (s *Store) finishRunningTask(ctx context.Context, taskID, status, resultJSON string, lease *OperationLease) error {
	return s.submit(ctx, func(db *dialectDB) error {
		query := `
			UPDATE task SET status=?, result_json=?, finished_at=?
			 WHERE task_id=? AND status='running'`
		args := []any{status, resultJSON, s.now().Unix(), taskID}
		if lease != nil {
			query += `
			   AND EXISTS (
			       SELECT 1 FROM operation_lease
			        WHERE resource_type=? AND resource_id=? AND holder_id=? AND token=?`
			args = append(args, lease.Resource, lease.ResourceID, lease.HolderID, lease.Token)
			if db.providerName() == ProviderPostgres {
				query += `
			          AND expires_at > (SELECT FLOOR(EXTRACT(EPOCH FROM clock_timestamp()) * 1000)::BIGINT)
			   )`
			} else {
				query += `
			          AND expires_at > ?
			   )`
				args = append(args, s.now().UTC().UnixMilli())
			}
		}
		result, err := db.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			if lease != nil {
				return ErrLeaseLost
			}
			return ErrTaskNotRunning
		}
		return nil
	})
}

// GetTask returns one task row, or ErrNotFound.
func (s *Store) GetTask(ctx context.Context, taskID string) (*Task, error) {
	return scanTask(s.db.QueryRowContext(ctx,
		`SELECT `+taskSelectCols+` FROM task WHERE task_id=?`, taskID))
}

// ListTasksForSandbox returns a sandbox's tasks, newest first (task_id is a
// ULID, so id order is chronological), capped at limit. Durable — works after
// the sandbox has stopped or been destroyed.
func (s *Store) ListTasksForSandbox(ctx context.Context, sandboxID string, limit int) ([]*Task, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+taskSelectCols+`
		  FROM task WHERE sandbox_id=? ORDER BY task_id DESC LIMIT ?`, sandboxID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListRunningTasks returns every legacy runtimed task row still in the
// `running` state. Hosted Copilot turns have their own control-plane recovery
// path and must not be attached to a runtimed event stream.
func (s *Store) ListRunningTasks(ctx context.Context) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+taskSelectCols+`
		  FROM task WHERE status='running' AND execution_kind=?`, TaskExecutionRuntimed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SandboxHasRunningTask reports whether the sandbox has active execution. A
// waiting hosted interaction uses waiting_* instead, so idle reaping may stop
// the sandbox while a human considers a question.
func (s *Store) SandboxHasRunningTask(ctx context.Context, sandboxID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM task WHERE sandbox_id=? AND status IN ('running','cancelling'))`,
		sandboxID).Scan(&n)
	return n == 1, err
}

// SandboxHasBusyConversation prevents legacy task admission while a hosted
// turn owns the workspace, including a paused question or a queued follow-up.
func (s *Store) SandboxHasBusyConversation(ctx context.Context, sandboxID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM conversation c
			 WHERE c.sandbox_id=? AND c.archived_at IS NULL
			   AND (
				c.active_turn_id IS NOT NULL OR EXISTS(
					SELECT 1 FROM conversation_turn t
					 WHERE t.conversation_id=c.id AND t.status='queued'
				)
			   )
		)`, sandboxID).Scan(&n)
	return n == 1, err
}
