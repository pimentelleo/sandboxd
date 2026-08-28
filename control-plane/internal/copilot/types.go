// Package copilot hosts GitHub Copilot outside sandbox containers.
package copilot

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/secrets"
)

const (
	workspaceDir = "/home/sandbox/workspace/app"
	maxPrompt    = 128 << 10
	maxSystem    = 32 << 10

	ConversationModeInteractive = "interactive"
	ConversationModePlan        = "plan"
	ConversationModeAutopilot   = "autopilot"
)

var (
	ErrInvalidPAT              = errors.New("invalid GitHub Copilot personal access token")
	ErrCredentialChanged       = errors.New("GitHub Copilot connection changed")
	ErrNotConnected            = errors.New("GitHub Copilot is not connected")
	ErrCapability              = errors.New("invalid or expired task capability")
	ErrInvalidModelSelection   = errors.New("invalid GitHub Copilot model selection")
	ErrModelCatalogUnavailable = errors.New("GitHub Copilot model catalog is unavailable")
	ErrSessionError            = errors.New("Copilot session reported an error")
)

// Config configures the hosted Copilot provider. StateDir must be a private
// control-plane data directory, never a sandbox workspace.
type Config struct {
	StateDir string
	Cipher   *secrets.Cipher
	// Executor is the bounded Docker adapter. The parent integration supplies an
	// adapter around docker.Client.ExecScoped; it is kept separate to make this
	// package testable without a Docker daemon.
	Executor   ScopedExecutor
	Log        *slog.Logger
	HTTPClient *http.Client

	UserURL string
	Now     func() time.Time
	Runtime RuntimeClient
}

// Status deliberately contains no credential material.
type Status struct {
	Connected bool   `json:"connected"`
	Account   string `json:"account,omitempty"`
	Method    string `json:"method,omitempty"`
}

// TaskRequest is the private bridge request payload.
type TaskRequest struct {
	Capability string `json:"capability"`
	Prompt     string `json:"prompt"`
	Model      string `json:"model"`
	// Continue is intentionally tri-state. Nil lets the manager resume the
	// sandbox's durable Copilot session when one exists.
	Continue     *bool  `json:"continue,omitempty"`
	SystemPrompt string `json:"system_prompt"`
}

// Envelope is the only event shape emitted from the private bridge.
type Envelope struct {
	Type       string `json:"type"`
	Role       string `json:"role,omitempty"`
	Text       string `json:"text,omitempty"`
	Name       string `json:"name,omitempty"`
	Status     string `json:"status,omitempty"`
	Path       string `json:"path,omitempty"`
	Input      int64  `json:"input,omitempty"`
	Output     int64  `json:"output,omitempty"`
	Reasoning  int64  `json:"reasoning,omitempty"`
	CacheRead  int64  `json:"cache_read,omitempty"`
	CacheWrite int64  `json:"cache_write,omitempty"`
	Total      int64  `json:"total,omitempty"`
	Message    string `json:"message,omitempty"`
}

// ScopedExecRequest is intentionally bounded. The Docker integration must
// execute it without a TTY and without inheriting control-plane credentials.
type ScopedExecRequest struct {
	Container   string
	User        string
	Workdir     string
	Command     []string
	Stdin       []byte
	Timeout     time.Duration
	OutputLimit int
}

type ScopedExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// ScopedExecutor is adapted by the control-plane Docker package. Its matching
// target is ExecScoped(ctx, docker.ScopedExecRequest{Container, User, Workdir,
// Command, Stdin, Timeout, OutputLimit}).
type ScopedExecutor interface {
	ExecScoped(context.Context, ScopedExecRequest) (ScopedExecResult, error)
}

// ScopedExecutorFunc adapts a function for Config.Executor.
type ScopedExecutorFunc func(context.Context, ScopedExecRequest) (ScopedExecResult, error)

func (f ScopedExecutorFunc) ExecScoped(ctx context.Context, request ScopedExecRequest) (ScopedExecResult, error) {
	return f(ctx, request)
}

// RuntimeClient is a small seam around the official SDK so tests make no SDK
// process, network, or Docker calls.
type RuntimeClient interface {
	Create(context.Context, RuntimeConfig) (RuntimeSession, error)
	Resume(context.Context, string, RuntimeConfig) (RuntimeSession, error)
	Delete(context.Context, string) error
}

// ModelInfo is the allowlisted subset of SDK model metadata that may be
// returned to an authenticated console client. It intentionally excludes
// policy, billing, capabilities, and all SDK transport details.
type ModelInfo struct {
	ID                        string   `json:"id"`
	Name                      string   `json:"name"`
	SupportedReasoningEfforts []string `json:"supported_reasoning_efforts"`
	DefaultReasoningEffort    string   `json:"default_reasoning_effort,omitempty"`
	MaxContextWindowTokens    *int     `json:"max_context_window_tokens,omitempty"`
}

// ModelCatalogRuntime is an optional extension of RuntimeClient. Keeping it
// separate preserves the existing runtime test seam and legacy task adapter.
type ModelCatalogRuntime interface {
	ListModels(context.Context, string) ([]ModelInfo, error)
	InvalidateModelCatalog()
}

// ModelSelection is the normalized, immutable selection stored with a
// conversation turn. An empty Model delegates model selection to the provider.
type ModelSelection struct {
	Model           string
	ReasoningEffort string
	ContextTier     string
}

type RuntimeSession interface {
	ID() string
	Send(context.Context, string) error
	Abort(context.Context) error
	Disconnect() error
}

// ConversationRuntimeSession is the optional interactive extension of
// RuntimeSession. Keeping it separate preserves the legacy bridge seam for
// existing runtime fakes and callers.
type ConversationRuntimeSession interface {
	RuntimeSession
	SendMessage(context.Context, RuntimeMessage) error
}

type RuntimeConfig struct {
	Model               string
	ReasoningEffort     string
	ContextTier         string
	Token               string
	SystemPrompt        string
	Workdir             string
	OnEvent             func(RuntimeEvent)
	OnUserInputRequest  func(RuntimeInputRequest) (RuntimeInputResponse, error)
	OnPlanRequest       func(RuntimePlanRequest) (RuntimePlanResponse, error)
	ContinuePendingWork bool
	Tools               []RuntimeTool
}

// BackgroundTaskRequest is the immutable parent context supplied to the
// control-plane delegation harness. The custom tool schema exposes only Task
// and Label, never identifiers or workspace/container paths.
type BackgroundTaskRequest struct {
	ConversationID string
	TurnID         string
	SandboxID      string
	Task           string
	Label          string
}

// BackgroundTask is the safe, bounded projection a parent Copilot turn may
// inspect. Worker workspace paths, container names, and SDK session IDs never
// leave the control plane.
type BackgroundTask struct {
	ID              string   `json:"id"`
	ParentTurnID    string   `json:"parent_turn_id"`
	Label           string   `json:"label,omitempty"`
	Task            string   `json:"task"`
	Model           string   `json:"model,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	ContextTier     string   `json:"context_tier"`
	Status          string   `json:"status"`
	Result          string   `json:"result,omitempty"`
	ErrorMessage    string   `json:"error_message,omitempty"`
	PatchState      string   `json:"patch_state"`
	ChangedFiles    []string `json:"changed_files"`
}

// BackgroundTaskChange is read one file at a time so a large delegated patch
// cannot flood a parent model context. The parent decides whether to use its
// regular write_file tool to apply this content.
type BackgroundTaskChange struct {
	TaskID     string `json:"task_id"`
	Path       string `json:"path"`
	BaseSHA256 string `json:"base_sha256,omitempty"`
	Content    string `json:"content,omitempty"`
	Deleted    bool   `json:"deleted,omitempty"`
	Mode       uint32 `json:"mode,omitempty"`
}

// BackgroundDelegate is implemented by the durable API coordinator. It keeps
// isolated child execution out of the Copilot package while allowing the
// parent-only tool surface to remain strongly typed and testable.
type BackgroundDelegate interface {
	SpawnBackgroundTask(context.Context, BackgroundTaskRequest) (BackgroundTask, error)
	ListBackgroundTasks(context.Context, string) ([]BackgroundTask, error)
	GetBackgroundTask(context.Context, string, string) (BackgroundTask, error)
	ReadBackgroundTaskChange(context.Context, string, string, string) (BackgroundTaskChange, error)
	CancelBackgroundTask(context.Context, string, string) (BackgroundTask, error)
}

// RuntimeMessage captures the selected native Copilot agent mode for one turn.
// Mode is one of plan, interactive, or autopilot.
type RuntimeMessage struct {
	Prompt string
	Mode   string
}

// RuntimeInputRequest and RuntimePlanRequest deliberately expose only data
// intended for the hosted UI. RequestID remains an opaque provider identifier
// and never enters public API or transcript records.
type RuntimeInputRequest struct {
	RequestID     string
	Question      string
	Choices       []string
	AllowFreeform bool
}

type RuntimeInputResponse struct {
	Answer      string
	WasFreeform bool
}

type RuntimePlanRequest struct {
	RequestID         string
	Summary           string
	Plan              string
	Actions           []string
	RecommendedAction string
}

type RuntimePlanResponse struct {
	Approved       bool
	SelectedAction string
	Feedback       string
}

type RuntimeTool struct {
	Name        string
	Description string
	Schema      map[string]any
	Handler     func(any) (string, error)
}

type RuntimeEvent struct {
	Type       string
	Text       string
	ToolCallID string
	ToolName   string
	Success    bool
	Input      int64
	Output     int64
	Reasoning  int64
	CacheRead  int64
	CacheWrite int64
	// Interaction fields are emitted so the adapter can safely correlate a
	// direct SDK callback, which lacks a request ID, with the matching event.
	RequestID         string
	Question          string
	Choices           []string
	AllowFreeform     bool
	Summary           string
	Plan              string
	Actions           []string
	RecommendedAction string
}
