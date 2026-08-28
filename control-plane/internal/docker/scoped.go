package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ScopedExecRequest is a non-interactive, bounded docker-exec invocation.
// Callers must explicitly supply the target container, user, workdir, deadline,
// and output limit so the security boundary is visible at each call site.
type ScopedExecRequest struct {
	Container   string
	User        string
	Workdir     string
	Command     []string
	Stdin       []byte
	Timeout     time.Duration
	OutputLimit int
}

// ExecScoped executes one bounded command in a specific container. It never
// allocates a TTY, always fixes the container user and working directory, and
// retains at most OutputLimit bytes across stdout and stderr.
func (c *Client) ExecScoped(ctx context.Context, request ScopedExecRequest) (ExecResult, error) {
	if c == nil || c.Bin == "" {
		return ExecResult{}, errors.New("docker exec: client is unavailable")
	}
	if err := validateScopedExec(request); err != nil {
		return ExecResult{}, err
	}

	execCtx, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()

	args := scopedExecArgs(request)
	command := exec.CommandContext(execCtx, c.Bin, args...)
	if len(request.Stdin) > 0 {
		command.Stdin = bytes.NewReader(request.Stdin)
	}
	output := newBoundedExecOutput(request.OutputLimit)
	command.Stdout = output.stdoutWriter()
	command.Stderr = output.stderrWriter()
	err := command.Run()
	result := ExecResult{
		Stdout:   output.stdout.String(),
		Stderr:   output.stderr.String(),
		ExitCode: 0,
	}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return result, context.DeadlineExceeded
	}
	return result, fmt.Errorf("docker exec: %w", err)
}

func validateScopedExec(request ScopedExecRequest) error {
	if !safeDockerExecName(request.Container) {
		return errors.New("docker exec: invalid container")
	}
	if !safeDockerExecUser(request.User) {
		return errors.New("docker exec: invalid user")
	}
	if !strings.HasPrefix(request.Workdir, "/") || strings.ContainsAny(request.Workdir, "\x00\r\n") {
		return errors.New("docker exec: invalid workdir")
	}
	if len(request.Command) == 0 {
		return errors.New("docker exec: command is required")
	}
	for _, arg := range request.Command {
		if strings.ContainsRune(arg, 0) {
			return errors.New("docker exec: command contains NUL")
		}
	}
	if request.Timeout <= 0 {
		return errors.New("docker exec: timeout is required")
	}
	if request.OutputLimit <= 0 {
		return errors.New("docker exec: output limit is required")
	}
	return nil
}

func safeDockerExecName(value string) bool {
	if value == "" || len(value) > 255 {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' {
			if i == 0 && r == '-' {
				return false
			}
			continue
		}
		return false
	}
	return true
}

func safeDockerExecUser(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == ':' {
			continue
		}
		return false
	}
	return true
}

func scopedExecArgs(request ScopedExecRequest) []string {
	args := []string{"exec", "--user", request.User, "--workdir", request.Workdir}
	if len(request.Stdin) > 0 {
		args = append(args, "--interactive")
	}
	seconds := int(math.Ceil(request.Timeout.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	// coreutils `timeout` is available in sandboxd's Debian base image. It
	// terminates an in-container command even if the control-plane client exits.
	args = append(args, request.Container, "timeout", "--signal=TERM",
		"--kill-after=5s", "--", fmt.Sprintf("%ds", seconds))
	return append(args, request.Command...)
}

type boundedExecOutput struct {
	mu        sync.Mutex
	remaining int
	stdout    bytes.Buffer
	stderr    bytes.Buffer
}

func newBoundedExecOutput(limit int) *boundedExecOutput {
	return &boundedExecOutput{remaining: limit}
}

func (o *boundedExecOutput) stdoutWriter() *boundedExecWriter {
	return &boundedExecWriter{output: o, buffer: &o.stdout}
}

func (o *boundedExecOutput) stderrWriter() *boundedExecWriter {
	return &boundedExecWriter{output: o, buffer: &o.stderr}
}

type boundedExecWriter struct {
	output *boundedExecOutput
	buffer *bytes.Buffer
}

func (w *boundedExecWriter) Write(value []byte) (int, error) {
	w.output.mu.Lock()
	defer w.output.mu.Unlock()
	if w.output.remaining > 0 {
		n := len(value)
		if n > w.output.remaining {
			n = w.output.remaining
		}
		_, _ = w.buffer.Write(value[:n])
		w.output.remaining -= n
	}
	// Report that all bytes were consumed so os/exec keeps draining the pipe
	// instead of blocking the sandbox command once the retained output is full.
	return len(value), nil
}
