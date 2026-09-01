package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// RemoteExecRequest identifies exactly one in-pod exec stream. The adapter
// always selects a fixed container name; callers never provide pod names.
type RemoteExecRequest struct {
	Namespace string
	Pod       string
	Container string
	Command   []string
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	TTY       bool
	Resize    remotecommand.TerminalSizeQueue
}

// RemoteExecutor is injectable because client-go's SPDY/WebSocket transport
// cannot be exercised by unit tests without an API server.
type RemoteExecutor interface {
	Stream(context.Context, RemoteExecRequest) error
}

// RemoteCommandExecutor executes through Kubernetes' websocket-first exec
// protocol and falls back to SPDY only for upgrade/proxy negotiation errors.
type RemoteCommandExecutor struct {
	config *rest.Config
	core   corev1client.CoreV1Interface
}

var _ RemoteExecutor = (*RemoteCommandExecutor)(nil)

// NewRemoteCommandExecutor creates the real client-go exec transport.
func NewRemoteCommandExecutor(config *rest.Config, core corev1client.CoreV1Interface) (*RemoteCommandExecutor, error) {
	if config == nil || core == nil {
		return nil, ErrClientRequired
	}
	return &RemoteCommandExecutor{config: rest.CopyConfig(config), core: core}, nil
}

func (e *RemoteCommandExecutor) Stream(ctx context.Context, request RemoteExecRequest) error {
	if request.Namespace == "" || request.Pod == "" || request.Container == "" || len(request.Command) == 0 {
		return fmt.Errorf("%w: exec target and command are required", ErrUnsafeSandboxSpec)
	}
	execURL := e.core.RESTClient().Post().
		Resource("pods").
		Namespace(request.Namespace).
		Name(request.Pod).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: request.Container,
			Command:   request.Command,
			Stdin:     request.Stdin != nil,
			Stdout:    request.Stdout != nil,
			Stderr:    request.Stderr != nil && !request.TTY,
			TTY:       request.TTY,
		}, scheme.ParameterCodec).
		URL()

	websocketExecutor, err := remotecommand.NewWebSocketExecutor(e.config, http.MethodGet, execURL.String())
	if err != nil {
		return fmt.Errorf("kubernetes runtime: create websocket exec transport: %w", err)
	}
	spdyExecutor, err := remotecommand.NewSPDYExecutor(e.config, http.MethodPost, execURL)
	if err != nil {
		return fmt.Errorf("kubernetes runtime: create SPDY exec transport: %w", err)
	}
	executor, err := remotecommand.NewFallbackExecutor(websocketExecutor, spdyExecutor, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return fmt.Errorf("kubernetes runtime: create fallback exec transport: %w", err)
	}
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             request.Stdin,
		Stdout:            request.Stdout,
		Stderr:            request.Stderr,
		Tty:               request.TTY,
		TerminalSizeQueue: request.Resize,
	})
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("kubernetes runtime: exec stream: %w", err)
}

type limitedBuffer struct {
	limit int
	data  []byte
}

func newLimitedBuffer(limit int) *limitedBuffer {
	return &limitedBuffer{limit: limit}
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if len(data) > b.limit-len(b.data) {
		return 0, ErrOutputLimitExceeded
	}
	b.data = append(b.data, data...)
	return len(data), nil
}

func (b *limitedBuffer) String() string { return string(b.data) }
func (b *limitedBuffer) Bytes() []byte  { return append([]byte(nil), b.data...) }
