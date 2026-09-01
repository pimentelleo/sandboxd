package runtimebackend

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/loopback"
	runtimed "github.com/tastyeffectco/sandboxd/control-plane/internal/runtime"
)

var (
	ErrDockerClientRequired              = errors.New("runtime backend: docker client is required")
	ErrWorkspaceManagerRequired          = errors.New("runtime backend: workspace manager is required")
	ErrRuntimeClientRequired             = errors.New("runtime backend: runtime client is required")
	ErrInvalidSandboxRef                 = errors.New("runtime backend: sandbox reference is required")
	ErrRuntimeNameRequired               = errors.New("runtime backend: runtime name is required")
	ErrRuntimeIDRequired                 = errors.New("runtime backend: runtime ID is required")
	ErrOwnershipNormalizationUnsupported = errors.New("runtime backend: ownership normalization is unsupported")
)

// TaskRuntimeBinder binds a per-sandbox private runtimed transport. Providers
// must keep that transport private; callers receive only this narrow contract.
type TaskRuntimeBinder interface {
	BindTaskRuntime(SandboxRef) TaskRuntime
}

// DockerClient is the Docker CLI subset used by DockerAdapter.
// *docker.Client satisfies this interface.
type DockerClient interface {
	Run(context.Context, docker.RunSpec) (string, error)
	Inspect(context.Context, string) (*docker.ContainerJSON, error)
	Remove(context.Context, string) error
	Start(context.Context, string) error
	Stop(context.Context, string, int) error
	Pause(context.Context, string) error
	Unpause(context.Context, string) error
	Exec(context.Context, string, []string) (docker.ExecResult, error)
	ExecScoped(context.Context, docker.ScopedExecRequest) (docker.ExecResult, error)
	ExecTTYContext(context.Context, string, string, string, []string) (*os.File, *exec.Cmd, error)
}

var _ DockerClient = (*docker.Client)(nil)

// DockerAdapter maps the Docker CLI's typed API to the provider-neutral
// lifecycle and execution contracts.
type DockerAdapter struct {
	client DockerClient
}

var (
	_ Lifecycle              = (*DockerAdapter)(nil)
	_ NonInteractiveExecutor = (*DockerAdapter)(nil)
	_ ScopedExecutor         = (*DockerAdapter)(nil)
	_ TTYExecutor            = (*DockerAdapter)(nil)
)

// NewDockerAdapter adapts a Docker client to the runtime backend contracts.
func NewDockerAdapter(client DockerClient) (*DockerAdapter, error) {
	if client == nil {
		return nil, ErrDockerClientRequired
	}
	return &DockerAdapter{client: client}, nil
}

func (a *DockerAdapter) Create(ctx context.Context, spec SandboxSpec) (Sandbox, error) {
	if spec.RuntimeName == "" {
		return Sandbox{}, ErrRuntimeNameRequired
	}
	containerID, err := a.client.Run(ctx, docker.RunSpec{
		Name:        spec.RuntimeName,
		Hostname:    spec.Hostname,
		Network:     spec.Network,
		Userns:      spec.UserNS,
		Runtime:     spec.Runtime,
		ReadOnly:    spec.ReadOnly,
		CapDrop:     spec.CapDrop,
		SecurityOpt: spec.SecurityOpt,
		CPUShares:   spec.CPUShares,
		Memory:      spec.Memory,
		MemorySwap:  spec.MemorySwap,
		PidsLimit:   spec.PidsLimit,
		Ulimits:     spec.Ulimits,
		Tmpfs:       spec.Tmpfs,
		Env:         spec.Env,
		Volumes:     spec.Volumes,
		Labels:      spec.Labels,
		Image:       spec.Image,
		Cmd:         spec.Command,
	})
	if err != nil {
		return Sandbox{}, err
	}
	return Sandbox{
		Ref:       SandboxRef{ID: spec.Ref.ID, RuntimeID: containerID},
		State:     LifecycleRunning,
		Image:     spec.Image,
		Labels:    labelsFromPairs(spec.Labels),
		ProcessID: 0,
	}, nil
}

func (a *DockerAdapter) Inspect(ctx context.Context, ref SandboxRef) (Sandbox, error) {
	target, err := dockerTarget(ref)
	if err != nil {
		return Sandbox{}, err
	}
	container, err := a.client.Inspect(ctx, target)
	if err != nil {
		return Sandbox{}, err
	}
	return sandboxFromDocker(ref, container), nil
}

func (a *DockerAdapter) Start(ctx context.Context, ref SandboxRef) error {
	target, err := dockerTarget(ref)
	if err != nil {
		return err
	}
	return a.client.Start(ctx, target)
}

func (a *DockerAdapter) Stop(ctx context.Context, ref SandboxRef, grace time.Duration) error {
	target, err := dockerTarget(ref)
	if err != nil {
		return err
	}
	seconds := int(grace.Seconds())
	if grace > 0 && seconds == 0 {
		seconds = 1
	}
	return a.client.Stop(ctx, target, seconds)
}

func (a *DockerAdapter) Pause(ctx context.Context, ref SandboxRef) error {
	target, err := dockerTarget(ref)
	if err != nil {
		return err
	}
	return a.client.Pause(ctx, target)
}

func (a *DockerAdapter) Unpause(ctx context.Context, ref SandboxRef) error {
	target, err := dockerTarget(ref)
	if err != nil {
		return err
	}
	return a.client.Unpause(ctx, target)
}

func (a *DockerAdapter) Remove(ctx context.Context, ref SandboxRef) error {
	target, err := dockerTarget(ref)
	if err != nil {
		return err
	}
	return a.client.Remove(ctx, target)
}

func (a *DockerAdapter) Exec(ctx context.Context, ref SandboxRef, command Command) (CommandResult, error) {
	target, err := dockerTarget(ref)
	if err != nil {
		return CommandResult{}, err
	}
	result, err := a.client.Exec(ctx, target, command.Args)
	return commandResultFromDocker(result), err
}

func (a *DockerAdapter) ExecScoped(ctx context.Context, ref SandboxRef, command ScopedCommand) (CommandResult, error) {
	target, err := dockerTarget(ref)
	if err != nil {
		return CommandResult{}, err
	}
	result, err := a.client.ExecScoped(ctx, docker.ScopedExecRequest{
		Container:   target,
		User:        command.User,
		Workdir:     command.Workdir,
		Command:     command.Args,
		Stdin:       command.Stdin,
		Timeout:     command.Timeout,
		OutputLimit: command.OutputLimit,
	})
	return commandResultFromDocker(result), err
}

func (a *DockerAdapter) OpenTTY(ctx context.Context, ref SandboxRef, request TTYRequest) (TTYSession, error) {
	target, err := dockerTarget(ref)
	if err != nil {
		return nil, err
	}
	terminal, command, err := a.client.ExecTTYContext(ctx, target, request.User, request.Workdir, request.Args)
	if err != nil {
		return nil, err
	}
	return &dockerTTYSession{terminal: terminal, command: command}, nil
}

func dockerTarget(ref SandboxRef) (string, error) {
	if ref.RuntimeID == "" {
		return "", ErrRuntimeIDRequired
	}
	return ref.RuntimeID, nil
}

func sandboxFromDocker(ref SandboxRef, container *docker.ContainerJSON) Sandbox {
	if ref.RuntimeID == "" {
		ref.RuntimeID = container.ID
	}
	return Sandbox{
		Ref:       ref,
		State:     dockerLifecycle(container),
		Image:     container.Config.Image,
		Labels:    container.Config.Labels,
		ProcessID: container.State.Pid,
	}
}

func dockerLifecycle(container *docker.ContainerJSON) LifecycleState {
	if container.State.Running {
		return LifecycleRunning
	}
	switch container.State.Status {
	case "created", "restarting":
		return LifecyclePending
	case "exited", "dead":
		return LifecycleStopped
	case "paused":
		return LifecyclePaused
	case "removing":
		return LifecycleDeleted
	default:
		return LifecycleUnknown
	}
}

func commandResultFromDocker(result docker.ExecResult) CommandResult {
	return CommandResult{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}
}

func labelsFromPairs(pairs []string) map[string]string {
	labels := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		labels[pair] = ""
		for i := 0; i < len(pair); i++ {
			if pair[i] == '=' {
				delete(labels, pair)
				labels[pair[:i]] = pair[i+1:]
				break
			}
		}
	}
	return labels
}

type dockerTTYSession struct {
	terminal *os.File
	command  *exec.Cmd
}

var _ TTYSession = (*dockerTTYSession)(nil)

func (s *dockerTTYSession) Read(p []byte) (int, error)  { return s.terminal.Read(p) }
func (s *dockerTTYSession) Write(p []byte) (int, error) { return s.terminal.Write(p) }
func (s *dockerTTYSession) Close() error                { return s.terminal.Close() }

func (s *dockerTTYSession) Resize(rows, columns uint16) error {
	return pty.Setsize(s.terminal, &pty.Winsize{Rows: rows, Cols: columns})
}

func (s *dockerTTYSession) Wait() error { return s.command.Wait() }

func (s *dockerTTYSession) Kill() error {
	if s.command.Process == nil {
		return errors.New("runtime backend: terminal process is unavailable")
	}
	return s.command.Process.Kill()
}

// LoopbackManager is the directory-workspace subset used by WorkspaceAdapter.
// *loopback.Manager satisfies this interface.
type LoopbackManager interface {
	Paths(string) (string, string)
	Provision(context.Context, string) error
	ProvisionFromTemplate(context.Context, string, string) error
	Release(context.Context, string) error
	ImgExists(string) bool
}

var _ LoopbackManager = (*loopback.Manager)(nil)
var _ WorkspaceOwnershipNormalizer = (*loopback.Manager)(nil)

// WorkspaceAdapter maps the current durable directory workspace manager to the
// provider-neutral workspace contract.
type WorkspaceAdapter struct {
	manager LoopbackManager
}

var _ WorkspaceManager = (*WorkspaceAdapter)(nil)
var _ WorkspaceOwnershipNormalizer = (*WorkspaceAdapter)(nil)

// NewWorkspaceAdapter adapts the current loopback workspace manager.
func NewWorkspaceAdapter(manager LoopbackManager) (*WorkspaceAdapter, error) {
	if manager == nil {
		return nil, ErrWorkspaceManagerRequired
	}
	return &WorkspaceAdapter{manager: manager}, nil
}

func (a *WorkspaceAdapter) Paths(ref SandboxRef) (WorkspacePaths, error) {
	id, err := workspaceID(ref)
	if err != nil {
		return WorkspacePaths{}, err
	}
	storage, mount := a.manager.Paths(id)
	return WorkspacePaths{Storage: storage, Mount: mount}, nil
}

func (a *WorkspaceAdapter) Provision(ctx context.Context, ref SandboxRef) error {
	id, err := workspaceID(ref)
	if err != nil {
		return err
	}
	return a.manager.Provision(ctx, id)
}

func (a *WorkspaceAdapter) ProvisionFromTemplate(ctx context.Context, ref SandboxRef, templatePath string) error {
	id, err := workspaceID(ref)
	if err != nil {
		return err
	}
	return a.manager.ProvisionFromTemplate(ctx, id, templatePath)
}

func (a *WorkspaceAdapter) Release(ctx context.Context, ref SandboxRef) error {
	id, err := workspaceID(ref)
	if err != nil {
		return err
	}
	return a.manager.Release(ctx, id)
}

func (a *WorkspaceAdapter) Exists(ref SandboxRef) (bool, error) {
	id, err := workspaceID(ref)
	if err != nil {
		return false, err
	}
	return a.manager.ImgExists(id), nil
}

func (a *WorkspaceAdapter) NormalizeOwnership(path string) error {
	normalizer, ok := a.manager.(WorkspaceOwnershipNormalizer)
	if !ok {
		return ErrOwnershipNormalizationUnsupported
	}
	return normalizer.NormalizeOwnership(path)
}

func workspaceID(ref SandboxRef) (string, error) {
	if ref.ID == "" {
		return "", ErrInvalidSandboxRef
	}
	return ref.ID, nil
}

// UnixRuntimeClient is the runtimed HTTP-over-Unix-socket subset used by
// UnixRuntimeAdapter. *runtime.Client satisfies this interface.
type UnixRuntimeClient interface {
	Status(context.Context) (*runtimed.Status, error)
	StartTask(context.Context, runtimed.StartTaskRequest) error
	PrepareHostedTask(context.Context, runtimed.PrepareHostedTaskRequest) (*runtimed.HostedTaskPreparation, error)
	FinalizeHostedTask(context.Context, runtimed.FinalizeHostedTaskRequest) (*runtimed.TaskResult, error)
	AbandonHostedTask(context.Context, runtimed.AbandonHostedTaskRequest) (*runtimed.TaskResult, error)
	CancelTask(context.Context, string) error
	RevertTask(context.Context, string) error
	TaskEvents(context.Context, string, int) (io.ReadCloser, error)
}

var _ UnixRuntimeClient = (*runtimed.Client)(nil)

// TaskRuntime controls tasks within one running sandbox.
type TaskRuntime interface {
	Status(context.Context) (*runtimed.Status, error)
	StartTask(context.Context, runtimed.StartTaskRequest) error
	PrepareHostedTask(context.Context, runtimed.PrepareHostedTaskRequest) (*runtimed.HostedTaskPreparation, error)
	FinalizeHostedTask(context.Context, runtimed.FinalizeHostedTaskRequest) (*runtimed.TaskResult, error)
	AbandonHostedTask(context.Context, runtimed.AbandonHostedTaskRequest) (*runtimed.TaskResult, error)
	CancelTask(context.Context, string) error
	RevertTask(context.Context, string) error
	TaskEvents(context.Context, string, int) (io.ReadCloser, error)
}

// UnixRuntimeAdapter exposes a Unix-socket runtimed client through TaskRuntime.
type UnixRuntimeAdapter struct {
	client UnixRuntimeClient
}

var _ TaskRuntime = (*UnixRuntimeAdapter)(nil)

// NewUnixRuntimeAdapter adapts the existing runtimed Unix-socket client.
func NewUnixRuntimeAdapter(client UnixRuntimeClient) (*UnixRuntimeAdapter, error) {
	if client == nil {
		return nil, ErrRuntimeClientRequired
	}
	return &UnixRuntimeAdapter{client: client}, nil
}

func (a *UnixRuntimeAdapter) Status(ctx context.Context) (*runtimed.Status, error) {
	return a.client.Status(ctx)
}

func (a *UnixRuntimeAdapter) StartTask(ctx context.Context, request runtimed.StartTaskRequest) error {
	return a.client.StartTask(ctx, request)
}

func (a *UnixRuntimeAdapter) PrepareHostedTask(ctx context.Context, request runtimed.PrepareHostedTaskRequest) (*runtimed.HostedTaskPreparation, error) {
	return a.client.PrepareHostedTask(ctx, request)
}

func (a *UnixRuntimeAdapter) FinalizeHostedTask(ctx context.Context, request runtimed.FinalizeHostedTaskRequest) (*runtimed.TaskResult, error) {
	return a.client.FinalizeHostedTask(ctx, request)
}

func (a *UnixRuntimeAdapter) AbandonHostedTask(ctx context.Context, request runtimed.AbandonHostedTaskRequest) (*runtimed.TaskResult, error) {
	return a.client.AbandonHostedTask(ctx, request)
}

func (a *UnixRuntimeAdapter) CancelTask(ctx context.Context, taskID string) error {
	return a.client.CancelTask(ctx, taskID)
}

func (a *UnixRuntimeAdapter) RevertTask(ctx context.Context, taskID string) error {
	return a.client.RevertTask(ctx, taskID)
}

func (a *UnixRuntimeAdapter) TaskEvents(ctx context.Context, taskID string, since int) (io.ReadCloser, error) {
	return a.client.TaskEvents(ctx, taskID, since)
}
