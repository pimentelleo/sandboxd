package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtimebackend"
)

type fileOperation string

const (
	fileList    fileOperation = "list"
	fileRead    fileOperation = "read"
	fileWrite   fileOperation = "write"
	fileDelete  fileOperation = "delete"
	fileTailLog fileOperation = "tail_log"
)

var processLogName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// fileRequest is the protocol sent to runtimed-tunnel inside the sandbox. It
// redundantly carries only logical paths and is validated by both ends.
type fileRequest struct {
	Operation fileOperation `json:"operation"`
	Path      string        `json:"path"`
	Process   string        `json:"process,omitempty"`
	Contents  []byte        `json:"contents,omitempty"`
	MaxBytes  int64         `json:"max_bytes,omitempty"`
	Recursive bool          `json:"recursive,omitempty"`
	Limit     int           `json:"limit,omitempty"`
}

type fileResponse struct {
	Contents  []byte          `json:"contents,omitempty"`
	Lines     []string        `json:"lines,omitempty"`
	Info      *fileInfo       `json:"info,omitempty"`
	Entries   []fileInfo      `json:"entries,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
	Error     *fileErrorReply `json:"error,omitempty"`
}

type fileInfo struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type fileErrorReply struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ListFiles lists only entries under the sandbox's mounted PVC. The control
// plane never mounts or reads the volume directly.
func (a *Adapter) ListFiles(ctx context.Context, ref runtimebackend.SandboxRef, request runtimebackend.ListFilesRequest) (runtimebackend.ListFilesResult, error) {
	if request.Limit < 1 || request.Limit > a.config.MaxFileEntries {
		return runtimebackend.ListFilesResult{}, fmt.Errorf("%w: list limit must be between 1 and %d", ErrUnsafeSandboxSpec, a.config.MaxFileEntries)
	}
	response, err := a.fileRequest(ctx, ref, fileRequest{
		Operation: fileList, Path: request.Path.String(), Recursive: request.Recursive, Limit: request.Limit,
	})
	if err != nil {
		return runtimebackend.ListFilesResult{}, err
	}
	if err := response.error(); err != nil {
		return runtimebackend.ListFilesResult{}, err
	}
	result := runtimebackend.ListFilesResult{Entries: make([]runtimebackend.FileInfo, 0, len(response.Entries)), Truncated: response.Truncated}
	for _, entry := range response.Entries {
		info, err := runtimeFileInfo(entry)
		if err != nil {
			return runtimebackend.ListFilesResult{}, err
		}
		result.Entries = append(result.Entries, info)
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Path.String() < result.Entries[j].Path.String() })
	return result, nil
}

func (a *Adapter) ReadFile(ctx context.Context, ref runtimebackend.SandboxRef, request runtimebackend.ReadFileRequest) ([]byte, error) {
	if request.Path.IsRoot() {
		return nil, runtimebackend.ErrFileIsDirectory
	}
	if err := a.validateFileBytes(request.MaxBytes); err != nil {
		return nil, err
	}
	response, err := a.fileRequest(ctx, ref, fileRequest{Operation: fileRead, Path: request.Path.String(), MaxBytes: request.MaxBytes})
	if err != nil {
		return nil, err
	}
	if err := response.error(); err != nil {
		return nil, err
	}
	if int64(len(response.Contents)) > request.MaxBytes {
		return nil, runtimebackend.ErrFileLimitExceeded
	}
	return response.Contents, nil
}

func (a *Adapter) WriteFile(ctx context.Context, ref runtimebackend.SandboxRef, request runtimebackend.WriteFileRequest) (runtimebackend.FileInfo, error) {
	if request.Path.IsRoot() {
		return runtimebackend.FileInfo{}, runtimebackend.ErrFileIsDirectory
	}
	if err := a.validateFileBytes(request.MaxBytes); err != nil {
		return runtimebackend.FileInfo{}, err
	}
	if int64(len(request.Contents)) > request.MaxBytes {
		return runtimebackend.FileInfo{}, runtimebackend.ErrFileLimitExceeded
	}
	response, err := a.fileRequest(ctx, ref, fileRequest{
		Operation: fileWrite, Path: request.Path.String(), Contents: request.Contents, MaxBytes: request.MaxBytes,
	})
	if err != nil {
		return runtimebackend.FileInfo{}, err
	}
	if err := response.error(); err != nil {
		return runtimebackend.FileInfo{}, err
	}
	if response.Info == nil {
		return runtimebackend.FileInfo{}, fmt.Errorf("kubernetes runtime: file helper returned no file metadata")
	}
	return runtimeFileInfo(*response.Info)
}

func (a *Adapter) DeleteFile(ctx context.Context, ref runtimebackend.SandboxRef, logical runtimebackend.LogicalPath) error {
	if logical.IsRoot() {
		return runtimebackend.ErrFileIsDirectory
	}
	response, err := a.fileRequest(ctx, ref, fileRequest{Operation: fileDelete, Path: logical.String()})
	if err != nil {
		return err
	}
	return response.error()
}

// TailProcessLog reads one supervised-process log through the in-pod tunnel.
// Process is deliberately not a logical path: the helper accepts only a
// constrained process name and resolves the fixed .runtimed/<name>.log target.
func (a *Adapter) TailProcessLog(ctx context.Context, ref runtimebackend.SandboxRef, request runtimebackend.ProcessLogRequest) ([]string, error) {
	if !processLogName.MatchString(request.Process) {
		return nil, fmt.Errorf("%w: invalid process name", ErrUnsafeSandboxSpec)
	}
	if request.Tail < 1 || request.Tail > 1_000 {
		return nil, fmt.Errorf("%w: process log tail must be between 1 and 1000 lines", ErrUnsafeSandboxSpec)
	}
	if request.MaxBytes < 1 || request.MaxBytes > 256<<10 {
		return nil, fmt.Errorf("%w: process log read must be between 1 and 262144 bytes", ErrUnsafeSandboxSpec)
	}
	response, err := a.fileRequest(ctx, ref, fileRequest{
		Operation: fileTailLog,
		Process:   request.Process,
		Limit:     request.Tail,
		MaxBytes:  request.MaxBytes,
	})
	if err != nil {
		return nil, err
	}
	if err := response.error(); err != nil {
		return nil, err
	}
	if len(response.Lines) > request.Tail {
		return nil, runtimebackend.ErrFileLimitExceeded
	}
	return response.Lines, nil
}

func (a *Adapter) validateFileBytes(limit int64) error {
	if limit < 1 || limit > a.config.MaxFileBytes {
		return fmt.Errorf("%w: file limit must be between 1 and %d bytes", runtimebackend.ErrFileLimitExceeded, a.config.MaxFileBytes)
	}
	return nil
}

func (a *Adapter) fileRequest(ctx context.Context, ref runtimebackend.SandboxRef, input fileRequest) (fileResponse, error) {
	if a.executor == nil {
		return fileResponse{}, ErrExecutorRequired
	}
	meta, err := a.metadataForRef(ref)
	if err != nil {
		return fileResponse{}, err
	}
	pod, err := a.runningPod(ctx, meta)
	if err != nil {
		return fileResponse{}, err
	}
	body, err := json.Marshal(input)
	if err != nil {
		return fileResponse{}, err
	}
	ctx, cancel := a.withTimeout(ctx, a.config.Timeouts.File)
	defer cancel()
	// Base64 expands contents by 4/3; this headroom also bounds malformed
	// helper replies before json.Unmarshal sees them.
	limit := int(a.config.MaxFileBytes*2 + 64*1024)
	stdout, stderr := newLimitedBuffer(limit), newLimitedBuffer(16<<10)
	err = a.executor.Stream(ctx, RemoteExecRequest{
		Namespace: pod.Namespace,
		Pod:       pod.Name,
		Container: sandboxContainer,
		Command:   []string{"runtimed-tunnel", "file", "--root", a.config.WorkspaceMount},
		Stdin:     bytes.NewReader(body),
		Stdout:    stdout,
		Stderr:    stderr,
	})
	if err != nil {
		if stderr.String() != "" {
			return fileResponse{}, fmt.Errorf("kubernetes runtime: workspace file helper: %w: %s", err, stderr.String())
		}
		return fileResponse{}, err
	}
	var response fileResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		return fileResponse{}, fmt.Errorf("kubernetes runtime: decode workspace file helper response: %w", err)
	}
	return response, nil
}

func (response fileResponse) error() error {
	if response.Error == nil {
		return nil
	}
	switch response.Error.Code {
	case "not_found":
		return runtimebackend.ErrFileNotFound
	case "is_directory":
		return runtimebackend.ErrFileIsDirectory
	case "limit_exceeded":
		return runtimebackend.ErrFileLimitExceeded
	case "symlink_not_allowed":
		return runtimebackend.ErrSymlinkNotAllowed
	default:
		return fmt.Errorf("kubernetes runtime: workspace file helper: %s", response.Error.Message)
	}
}

func runtimeFileInfo(info fileInfo) (runtimebackend.FileInfo, error) {
	logical, err := runtimebackend.ParseLogicalPath(info.Path)
	if err != nil || logical.IsRoot() {
		return runtimebackend.FileInfo{}, fmt.Errorf("kubernetes runtime: helper returned invalid file path %q", info.Path)
	}
	fileType := runtimebackend.FileType(info.Type)
	if fileType != runtimebackend.FileTypeRegular && fileType != runtimebackend.FileTypeDirectory {
		return runtimebackend.FileInfo{}, fmt.Errorf("kubernetes runtime: helper returned invalid file type %q", info.Type)
	}
	if info.Size < 0 {
		return runtimebackend.FileInfo{}, fmt.Errorf("kubernetes runtime: helper returned negative file size")
	}
	return runtimebackend.FileInfo{Path: logical, Type: fileType, Size: info.Size}, nil
}

// GitOperation is intentionally a small allow-list rather than a generic
// container exec API. Credentials supplied on Stdin are transported only to
// the git-ops sidecar, which has no credential volume or environment.
type GitOperation string

const (
	GitFetch  GitOperation = "fetch"
	GitPush   GitOperation = "push"
	GitStatus GitOperation = "status"
)

// GitRequest is the future integration seam for credential-aware Git work.
// Stdin may carry a short-lived credential protocol payload and is never
// written to a pod environment, ConfigMap, Secret, or sandbox container.
type GitRequest struct {
	Operation GitOperation
	Args      []string
	Stdin     io.Reader
	Timeout   time.Duration
}

// ExecGit executes an allow-listed git command in the dedicated sidecar.
func (a *Adapter) ExecGit(ctx context.Context, ref runtimebackend.SandboxRef, request GitRequest) (runtimebackend.CommandResult, error) {
	switch request.Operation {
	case GitFetch, GitPush, GitStatus:
	default:
		return runtimebackend.CommandResult{}, fmt.Errorf("%w: unsupported git operation", ErrUnsafeSandboxSpec)
	}
	for _, argument := range request.Args {
		if argument == "" || strings.HasPrefix(argument, "-") {
			return runtimebackend.CommandResult{}, fmt.Errorf("%w: git arguments cannot be empty or options", ErrUnsafeSandboxSpec)
		}
	}
	timeout := request.Timeout
	if timeout == 0 {
		timeout = a.config.Timeouts.Exec
	}
	if timeout < time.Second || timeout > a.config.Timeouts.Exec {
		return runtimebackend.CommandResult{}, fmt.Errorf("%w: git timeout exceeds provider policy", ErrUnsafeSandboxSpec)
	}
	args := append([]string{"git", string(request.Operation)}, request.Args...)
	return a.execWithTimeout(ctx, ref, gitOpsContainer, args, request.Stdin, defaultExecOutputLimit, timeout)
}
