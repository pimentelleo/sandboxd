package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/utils/exec"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtimebackend"
)

const defaultExecOutputLimit = 1 << 20

// Exec runs a non-interactive command in the sandbox container.
func (a *Adapter) Exec(ctx context.Context, ref runtimebackend.SandboxRef, command runtimebackend.Command) (runtimebackend.CommandResult, error) {
	if len(command.Args) == 0 {
		return runtimebackend.CommandResult{}, fmt.Errorf("%w: command is required", ErrUnsafeSandboxSpec)
	}
	return a.exec(ctx, ref, sandboxContainer, command.Args, nil, defaultExecOutputLimit)
}

// ExecScoped preserves the provider contract without exposing Docker-specific
// flags. Pods run as the fixed non-root sandbox user, so another user is
// rejected instead of silently ignored.
func (a *Adapter) ExecScoped(ctx context.Context, ref runtimebackend.SandboxRef, command runtimebackend.ScopedCommand) (runtimebackend.CommandResult, error) {
	if len(command.Args) == 0 {
		return runtimebackend.CommandResult{}, fmt.Errorf("%w: command is required", ErrUnsafeSandboxSpec)
	}
	if command.User != "" && command.User != "sandbox" && command.User != "1000" {
		return runtimebackend.CommandResult{}, fmt.Errorf("%w: only the sandbox user can execute commands", ErrUnsafeSandboxSpec)
	}
	workdir, err := a.workspaceWorkdir(command.Workdir)
	if err != nil {
		return runtimebackend.CommandResult{}, err
	}
	limit := command.OutputLimit
	if limit == 0 {
		limit = defaultExecOutputLimit
	}
	if limit < 1 || limit > 8<<20 {
		return runtimebackend.CommandResult{}, fmt.Errorf("%w: output limit must be between 1 and 8 MiB", ErrUnsafeSandboxSpec)
	}
	timeout := command.Timeout
	if timeout == 0 {
		timeout = a.config.Timeouts.Exec
	}
	if timeout < time.Second || timeout > a.config.Timeouts.Exec {
		return runtimebackend.CommandResult{}, fmt.Errorf("%w: command timeout exceeds provider policy", ErrUnsafeSandboxSpec)
	}
	// The shell program is fixed and the user values are positional arguments,
	// so neither the working directory nor argv is interpolated into shell text.
	args := append([]string{"/bin/sh", "-c", `cd "$1" && shift && exec "$@"`, "sandboxd-exec", workdir}, command.Args...)
	return a.execWithTimeout(ctx, ref, sandboxContainer, args, bytes.NewReader(command.Stdin), limit, timeout)
}

func (a *Adapter) exec(ctx context.Context, ref runtimebackend.SandboxRef, container string, args []string, stdin io.Reader, outputLimit int) (runtimebackend.CommandResult, error) {
	return a.execWithTimeout(ctx, ref, container, args, stdin, outputLimit, a.config.Timeouts.Exec)
}

func (a *Adapter) execWithTimeout(ctx context.Context, ref runtimebackend.SandboxRef, container string, args []string, stdin io.Reader, outputLimit int, timeoutDuration time.Duration) (runtimebackend.CommandResult, error) {
	if a.executor == nil {
		return runtimebackend.CommandResult{}, ErrExecutorRequired
	}
	meta, err := a.metadataForRef(ref)
	if err != nil {
		return runtimebackend.CommandResult{}, err
	}
	pod, err := a.runningPod(ctx, meta)
	if err != nil {
		return runtimebackend.CommandResult{}, err
	}
	ctx, cancel := a.withTimeout(ctx, timeoutDuration)
	defer cancel()
	stdout, stderr := newLimitedBuffer(outputLimit), newLimitedBuffer(outputLimit)
	err = a.executor.Stream(ctx, RemoteExecRequest{
		Namespace: pod.Namespace,
		Pod:       pod.Name,
		Container: container,
		Command:   append([]string(nil), args...),
		Stdin:     stdin,
		Stdout:    stdout,
		Stderr:    stderr,
	})
	result := runtimebackend.CommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitError utilexec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitStatus()
		return result, nil
	}
	return result, err
}

func (a *Adapter) workspaceWorkdir(workdir string) (string, error) {
	if workdir == "" {
		return a.config.WorkspaceMount + "/workspace/app", nil
	}
	clean := path.Clean(workdir)
	if !strings.HasPrefix(clean, a.config.WorkspaceMount+"/") {
		return "", fmt.Errorf("%w: working directory must be under workspace mount", ErrUnsafeSandboxSpec)
	}
	return clean, nil
}

// OpenTTY opens a Kubernetes exec session and returns immediately. The
// in-cluster stream runs in the background; context cancellation or Close/Kill
// closes both pipe ends and unblocks all callers.
func (a *Adapter) OpenTTY(ctx context.Context, ref runtimebackend.SandboxRef, request runtimebackend.TTYRequest) (runtimebackend.TTYSession, error) {
	if a.executor == nil {
		return nil, ErrExecutorRequired
	}
	if len(request.Args) == 0 {
		return nil, fmt.Errorf("%w: terminal command is required", ErrUnsafeSandboxSpec)
	}
	if request.User != "" && request.User != "sandbox" && request.User != "1000" {
		return nil, fmt.Errorf("%w: only the sandbox user can execute commands", ErrUnsafeSandboxSpec)
	}
	workdir, err := a.workspaceWorkdir(request.Workdir)
	if err != nil {
		return nil, err
	}
	meta, err := a.metadataForRef(ref)
	if err != nil {
		return nil, err
	}
	pod, err := a.runningPod(ctx, meta)
	if err != nil {
		return nil, err
	}
	streamContext, cancel := context.WithCancel(ctx)
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	session := &kubernetesTTYSession{
		stdin:  stdinWriter,
		stdout: stdoutReader,
		cancel: cancel,
		sizes:  newTerminalSizeQueue(streamContext.Done()),
		done:   make(chan struct{}),
	}
	args := append([]string{"/bin/sh", "-c", `cd "$1" && shift && exec "$@"`, "sandboxd-tty", workdir}, request.Args...)
	go func() {
		err := a.executor.Stream(streamContext, RemoteExecRequest{
			Namespace: pod.Namespace,
			Pod:       pod.Name,
			Container: sandboxContainer,
			Command:   args,
			Stdin:     stdinReader,
			Stdout:    stdoutWriter,
			TTY:       true,
			Resize:    session.sizes,
		})
		_ = stdinReader.Close()
		if err != nil {
			_ = stdoutWriter.CloseWithError(err)
		} else {
			_ = stdoutWriter.Close()
		}
		session.resultMu.Lock()
		session.result = err
		session.resultMu.Unlock()
		close(session.done)
	}()
	return session, nil
}

type kubernetesTTYSession struct {
	stdin    *io.PipeWriter
	stdout   *io.PipeReader
	cancel   context.CancelFunc
	sizes    *terminalSizeQueue
	done     chan struct{}
	result   error
	resultMu sync.Mutex
	once     sync.Once
}

var _ runtimebackend.TTYSession = (*kubernetesTTYSession)(nil)

func (s *kubernetesTTYSession) Read(data []byte) (int, error)  { return s.stdout.Read(data) }
func (s *kubernetesTTYSession) Write(data []byte) (int, error) { return s.stdin.Write(data) }

func (s *kubernetesTTYSession) Close() error {
	s.once.Do(func() {
		s.cancel()
		_ = s.stdin.Close()
		_ = s.stdout.Close()
	})
	return nil
}

func (s *kubernetesTTYSession) Resize(rows, columns uint16) error {
	if rows == 0 || columns == 0 {
		return fmt.Errorf("%w: terminal dimensions must be non-zero", ErrUnsafeSandboxSpec)
	}
	return s.sizes.push(remotecommand.TerminalSize{Width: columns, Height: rows})
}

func (s *kubernetesTTYSession) Wait() error {
	<-s.done
	s.resultMu.Lock()
	err := s.result
	s.resultMu.Unlock()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (s *kubernetesTTYSession) Kill() error { return s.Close() }

type terminalSizeQueue struct {
	done <-chan struct{}
	ch   chan remotecommand.TerminalSize
}

func newTerminalSizeQueue(done <-chan struct{}) *terminalSizeQueue {
	return &terminalSizeQueue{done: done, ch: make(chan remotecommand.TerminalSize, 1)}
}

func (q *terminalSizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case <-q.done:
		return nil
	case size := <-q.ch:
		return &size
	}
}

func (q *terminalSizeQueue) push(size remotecommand.TerminalSize) error {
	select {
	case <-q.done:
		return context.Canceled
	default:
	}
	select {
	case q.ch <- size:
	default:
		select {
		case <-q.ch:
		default:
		}
		select {
		case q.ch <- size:
		case <-q.done:
			return context.Canceled
		}
	}
	return nil
}
