// Package runtimebackend defines provider-neutral control-plane contracts for
// sandbox runtimes. Providers implement these contracts without exposing their
// transport to callers.
package runtimebackend

import (
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"time"
)

// SandboxRef identifies one sandbox. ID is the stable control-plane identity;
// RuntimeID is the opaque provider handle required for operations after create.
type SandboxRef struct {
	ID        string
	RuntimeID string
}

// LifecycleState is the provider-neutral lifecycle state of a sandbox.
type LifecycleState string

const (
	LifecyclePending LifecycleState = "pending"
	LifecycleRunning LifecycleState = "running"
	LifecycleStopped LifecycleState = "stopped"
	LifecyclePaused  LifecycleState = "paused"
	LifecycleFailed  LifecycleState = "failed"
	LifecycleDeleted LifecycleState = "deleted"
	LifecycleUnknown LifecycleState = "unknown"
)

// Sandbox is the provider-neutral state returned by lifecycle operations.
type Sandbox struct {
	Ref       SandboxRef
	State     LifecycleState
	Image     string
	Labels    map[string]string
	ProcessID int
}

// SandboxSpec describes the runtime resources needed to create a sandbox.
// Providers may reject fields they cannot safely support.
type SandboxSpec struct {
	Ref SandboxRef
	// RuntimeName is the provider-selected stable name used when creating the
	// runtime object. It is deliberately separate from the logical sandbox ID.
	RuntimeName string
	Hostname    string
	Network     string
	UserNS      string
	Runtime     string
	ReadOnly    bool
	CapDrop     []string
	SecurityOpt []string
	CPUShares   int
	Memory      string
	MemorySwap  string
	PidsLimit   int
	Ulimits     []string
	Tmpfs       []string
	Env         []string
	Volumes     []string
	Labels      []string
	Image       string
	Command     []string
}

// Lifecycle creates and controls a sandbox instance.
type Lifecycle interface {
	Create(context.Context, SandboxSpec) (Sandbox, error)
	Inspect(context.Context, SandboxRef) (Sandbox, error)
	Start(context.Context, SandboxRef) error
	Stop(context.Context, SandboxRef, time.Duration) error
	Pause(context.Context, SandboxRef) error
	Unpause(context.Context, SandboxRef) error
	Remove(context.Context, SandboxRef) error
}

// PreviewTarget is a provider-owned, in-cluster endpoint for a sandbox preview.
// It is intentionally not a browser URL: only the control-plane gateway may
// connect to it after authorizing the external request.
type PreviewTarget struct {
	URL string
}

// PreviewGateway starts a sandbox when necessary and returns its private preview
// endpoint. Providers must never return a public or caller-controlled URL.
type PreviewGateway interface {
	EnsurePreview(context.Context, SandboxRef) (PreviewTarget, error)
}

// PreviewReadiness confirms that the provider's private preview endpoint is
// routable and serving before the gateway forwards browser traffic to it.
// It is separate from PreviewGateway so normal lifecycle and task operations
// never depend on an application's web-server readiness.
type PreviewReadiness interface {
	WaitForPreviewReady(context.Context, SandboxRef) error
}

// PurgeLifecycle is an opt-in irreversible extension to Lifecycle. Remove
// retains durable workspace state; callers must explicitly select Purge before
// a provider is allowed to delete that state.
type PurgeLifecycle interface {
	Purge(context.Context, SandboxRef) error
}

// WorkspacePaths identifies provider-managed durable workspace storage.
type WorkspacePaths struct {
	Storage string
	Mount   string
}

// WorkspaceManager creates and maintains durable workspaces independently of a
// sandbox's transient runtime instance.
type WorkspaceManager interface {
	Paths(SandboxRef) (WorkspacePaths, error)
	Provision(context.Context, SandboxRef) error
	ProvisionFromTemplate(context.Context, SandboxRef, string) error
	Release(context.Context, SandboxRef) error
	Exists(SandboxRef) (bool, error)
}

// WorkspaceOwnershipNormalizer is an optional host-filesystem capability. It
// is intentionally not required of workspace providers such as Kubernetes.
type WorkspaceOwnershipNormalizer interface {
	NormalizeOwnership(string) error
}

// LogicalPath is a validated slash-separated path relative to a workspace.
// The zero value represents the workspace root and is valid only for listing.
type LogicalPath struct {
	value string
}

// ParseLogicalPath validates a canonical, relative workspace path.
func ParseLogicalPath(value string) (LogicalPath, error) {
	if value == "" {
		return LogicalPath{}, nil
	}
	if strings.ContainsRune(value, 0) || strings.Contains(value, `\`) ||
		strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") {
		return LogicalPath{}, errors.New("runtime backend: invalid logical path")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return LogicalPath{}, errors.New("runtime backend: invalid logical path")
		}
	}
	if path.Clean(value) != value {
		return LogicalPath{}, errors.New("runtime backend: invalid logical path")
	}
	return LogicalPath{value: value}, nil
}

// String returns the canonical path relative to the workspace root.
func (p LogicalPath) String() string {
	return p.value
}

// IsRoot reports whether p identifies the workspace root.
func (p LogicalPath) IsRoot() bool {
	return p.value == ""
}

// FileType identifies an entry returned by WorkspaceFiles.
type FileType string

const (
	FileTypeRegular   FileType = "file"
	FileTypeDirectory FileType = "directory"
)

// FileInfo is metadata for a workspace file or directory.
type FileInfo struct {
	Path LogicalPath
	Type FileType
	Size int64
}

// ListFilesRequest bounds one workspace directory listing.
type ListFilesRequest struct {
	Path      LogicalPath
	Recursive bool
	Limit     int
}

// ListFilesResult contains at most the requested number of entries.
type ListFilesResult struct {
	Entries   []FileInfo
	Truncated bool
}

// ReadFileRequest bounds the bytes returned from one workspace file.
type ReadFileRequest struct {
	Path     LogicalPath
	MaxBytes int64
}

// WriteFileRequest atomically replaces one workspace file.
type WriteFileRequest struct {
	Path     LogicalPath
	Contents []byte
	MaxBytes int64
}

// WorkspaceFiles provides bounded workspace file operations using only
// validated logical paths; providers must not interpret them as host paths.
type WorkspaceFiles interface {
	ListFiles(context.Context, SandboxRef, ListFilesRequest) (ListFilesResult, error)
	ReadFile(context.Context, SandboxRef, ReadFileRequest) ([]byte, error)
	WriteFile(context.Context, SandboxRef, WriteFileRequest) (FileInfo, error)
	DeleteFile(context.Context, SandboxRef, LogicalPath) error
}

// ProcessLogRequest bounds a read of one runtimed-owned process log. Process
// is a provider-defined process name, not a workspace path; implementations
// must resolve it only beneath their private runtimed directory.
type ProcessLogRequest struct {
	Process  string
	Tail     int
	MaxBytes int64
}

// ProcessLogTailer reads a bounded tail of a runtimed process log without
// granting general access to the supervisor's private workspace subtree.
type ProcessLogTailer interface {
	TailProcessLog(context.Context, SandboxRef, ProcessLogRequest) ([]string, error)
}

// Command is a non-interactive command. Its result is collected after exit.
type Command struct {
	Args []string
}

// CommandResult is the captured result of a non-interactive command.
type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// NonInteractiveExecutor executes bounded, request/response commands without
// allocating a terminal.
type NonInteractiveExecutor interface {
	Exec(context.Context, SandboxRef, Command) (CommandResult, error)
}

// ScopedCommand is a non-interactive command with explicit execution bounds.
type ScopedCommand struct {
	User        string
	Workdir     string
	Args        []string
	Stdin       []byte
	Timeout     time.Duration
	OutputLimit int
}

// ScopedExecutor executes a command with an explicit identity, working
// directory, deadline, and output limit.
type ScopedExecutor interface {
	ExecScoped(context.Context, SandboxRef, ScopedCommand) (CommandResult, error)
}

// TTYRequest describes an interactive terminal command.
type TTYRequest struct {
	User    string
	Workdir string
	Args    []string
}

// TTYSession is an interactive byte stream. Its lifecycle is separate from
// non-interactive command execution because providers use different transports.
type TTYSession interface {
	io.ReadWriteCloser
	Resize(rows, columns uint16) error
	Wait() error
	Kill() error
}

// TTYExecutor opens an interactive terminal session.
type TTYExecutor interface {
	OpenTTY(context.Context, SandboxRef, TTYRequest) (TTYSession, error)
}
