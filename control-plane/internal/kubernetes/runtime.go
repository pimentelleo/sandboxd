package kubernetes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	runtimed "github.com/tastyeffectco/sandboxd/control-plane/internal/runtime"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtimebackend"
)

// StatusForSandbox fetches the runtimed status for ref.
func (a *Adapter) StatusForSandbox(ctx context.Context, ref runtimebackend.SandboxRef) (*runtimed.Status, error) {
	response, err := a.runtimedRequest(ctx, ref, http.MethodGet, "/status", nil, false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, runtimedResponseError("GET /status", response)
	}
	var status runtimed.Status
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("kubernetes runtime: decode runtimed status: %w", err)
	}
	return &status, nil
}

// StartTaskForSandbox starts one runtimed task over the private socket tunnel.
func (a *Adapter) StartTaskForSandbox(ctx context.Context, ref runtimebackend.SandboxRef, request runtimed.StartTaskRequest) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	response, err := a.runtimedRequest(ctx, ref, http.MethodPost, "/tasks", body, false)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusAccepted:
		return nil
	case http.StatusConflict:
		return runtimed.ErrTaskInProgress
	default:
		return runtimedResponseError("POST /tasks", response)
	}
}

func (a *Adapter) PrepareHostedTaskForSandbox(ctx context.Context, ref runtimebackend.SandboxRef, request runtimed.PrepareHostedTaskRequest) (*runtimed.HostedTaskPreparation, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	response, err := a.runtimedRequest(ctx, ref, http.MethodPost, "/hosted-tasks/prepare", body, false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		return nil, runtimed.ErrTaskInProgress
	}
	if response.StatusCode != http.StatusOK {
		return nil, runtimedResponseError("POST /hosted-tasks/prepare", response)
	}
	var preparation runtimed.HostedTaskPreparation
	if err := json.NewDecoder(response.Body).Decode(&preparation); err != nil {
		return nil, fmt.Errorf("kubernetes runtime: decode hosted task preparation: %w", err)
	}
	return &preparation, nil
}

func (a *Adapter) FinalizeHostedTaskForSandbox(ctx context.Context, ref runtimebackend.SandboxRef, request runtimed.FinalizeHostedTaskRequest) (*runtimed.TaskResult, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	path := "/hosted-tasks/" + url.PathEscape(request.TaskID) + "/finalize"
	return a.hostedResult(ctx, ref, http.MethodPost, path, body)
}

func (a *Adapter) AbandonHostedTaskForSandbox(ctx context.Context, ref runtimebackend.SandboxRef, request runtimed.AbandonHostedTaskRequest) (*runtimed.TaskResult, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	path := "/hosted-tasks/" + url.PathEscape(request.TaskID) + "/abandon"
	return a.hostedResult(ctx, ref, http.MethodPost, path, body)
}

func (a *Adapter) hostedResult(ctx context.Context, ref runtimebackend.SandboxRef, method, path string, body []byte) (*runtimed.TaskResult, error) {
	response, err := a.runtimedRequest(ctx, ref, method, path, body, false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		return nil, runtimed.ErrTaskInProgress
	}
	if response.StatusCode != http.StatusOK {
		return nil, runtimedResponseError(method+" "+path, response)
	}
	var result runtimed.TaskResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("kubernetes runtime: decode hosted task result: %w", err)
	}
	return &result, nil
}

func (a *Adapter) CancelTaskForSandbox(ctx context.Context, ref runtimebackend.SandboxRef, taskID string) error {
	response, err := a.runtimedRequest(ctx, ref, http.MethodPost, "/tasks/"+url.PathEscape(taskID)+"/cancel", nil, false)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return runtimedResponseError("POST /tasks/cancel", response)
	}
	return nil
}

func (a *Adapter) RevertTaskForSandbox(ctx context.Context, ref runtimebackend.SandboxRef, taskID string) error {
	response, err := a.runtimedRequest(ctx, ref, http.MethodPost, "/tasks/"+url.PathEscape(taskID)+"/revert", nil, false)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return runtimedResponseError("POST /tasks/revert", response)
	}
	return nil
}

// TaskEventsForSandbox returns runtimed's NDJSON event stream. Closing the
// returned body cancels the Kubernetes exec stream immediately.
func (a *Adapter) TaskEventsForSandbox(ctx context.Context, ref runtimebackend.SandboxRef, taskID string, since int) (io.ReadCloser, error) {
	response, err := a.runtimedRequest(ctx, ref, http.MethodGet,
		"/tasks/"+url.PathEscape(taskID)+"/events?since="+strconv.Itoa(since), nil, true)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return nil, runtimedResponseError("GET /tasks/events", response)
	}
	return response.Body, nil
}

// TaskRuntime is per-sandbox by design: the provider-neutral interface does
// not carry a SandboxRef in every method. BindTaskRuntime is the explicit
// bridge from a provider sandbox to that contract.
func (a *Adapter) BindTaskRuntime(ref runtimebackend.SandboxRef) runtimebackend.TaskRuntime {
	return &boundTaskRuntime{adapter: a, ref: ref}
}

type boundTaskRuntime struct {
	adapter *Adapter
	ref     runtimebackend.SandboxRef
}

var _ runtimebackend.TaskRuntime = (*boundTaskRuntime)(nil)

func (b *boundTaskRuntime) Status(ctx context.Context) (*runtimed.Status, error) {
	return b.adapter.StatusForSandbox(ctx, b.ref)
}
func (b *boundTaskRuntime) StartTask(ctx context.Context, request runtimed.StartTaskRequest) error {
	return b.adapter.StartTaskForSandbox(ctx, b.ref, request)
}
func (b *boundTaskRuntime) PrepareHostedTask(ctx context.Context, request runtimed.PrepareHostedTaskRequest) (*runtimed.HostedTaskPreparation, error) {
	return b.adapter.PrepareHostedTaskForSandbox(ctx, b.ref, request)
}
func (b *boundTaskRuntime) FinalizeHostedTask(ctx context.Context, request runtimed.FinalizeHostedTaskRequest) (*runtimed.TaskResult, error) {
	return b.adapter.FinalizeHostedTaskForSandbox(ctx, b.ref, request)
}
func (b *boundTaskRuntime) AbandonHostedTask(ctx context.Context, request runtimed.AbandonHostedTaskRequest) (*runtimed.TaskResult, error) {
	return b.adapter.AbandonHostedTaskForSandbox(ctx, b.ref, request)
}
func (b *boundTaskRuntime) CancelTask(ctx context.Context, taskID string) error {
	return b.adapter.CancelTaskForSandbox(ctx, b.ref, taskID)
}
func (b *boundTaskRuntime) RevertTask(ctx context.Context, taskID string) error {
	return b.adapter.RevertTaskForSandbox(ctx, b.ref, taskID)
}
func (b *boundTaskRuntime) TaskEvents(ctx context.Context, taskID string, since int) (io.ReadCloser, error) {
	return b.adapter.TaskEventsForSandbox(ctx, b.ref, taskID, since)
}

func (a *Adapter) runtimedRequest(ctx context.Context, ref runtimebackend.SandboxRef, method, path string, body []byte, stream bool) (*http.Response, error) {
	if a.executor == nil {
		return nil, ErrExecutorRequired
	}
	meta, err := a.metadataForRef(ref)
	if err != nil {
		return nil, err
	}
	pod, err := a.runningPod(ctx, meta)
	if err != nil {
		return nil, err
	}
	if !stream {
		timedContext, cancel := a.withTimeout(ctx, a.config.Timeouts.API)
		response, err := a.tunnelHTTP(timedContext, pod, method, path, body)
		if err != nil {
			cancel()
			return nil, err
		}
		response.Body = &cancelOnClose{ReadCloser: response.Body, cancel: cancel}
		return response, nil
	}
	return a.tunnelHTTP(ctx, pod, method, path, body)
}

func (a *Adapter) tunnelHTTP(ctx context.Context, pod *corev1.Pod, method, path string, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, "http://runtimed"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// runtimed-tunnel is a one-request bridge rather than a connection pool.
	// Closing the response deterministically releases the Kubernetes exec.
	request.Close = true
	var serialized bytes.Buffer
	if err := request.Write(&serialized); err != nil {
		return nil, fmt.Errorf("kubernetes runtime: encode tunneled request: %w", err)
	}

	streamContext, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	stderr := newLimitedBuffer(16 << 10)
	done := make(chan error, 1)
	go func() {
		err := a.executor.Stream(streamContext, RemoteExecRequest{
			Namespace: pod.Namespace,
			Pod:       pod.Name,
			Container: sandboxContainer,
			Command:   []string{"runtimed-tunnel", "stdio", "--socket", runtimed.DefaultSocketPath},
			Stdin:     bytes.NewReader(serialized.Bytes()),
			Stdout:    writer,
			Stderr:    stderr,
		})
		if err != nil {
			_ = writer.CloseWithError(err)
		} else {
			_ = writer.Close()
		}
		done <- err
	}()
	response, err := http.ReadResponse(bufio.NewReader(reader), request)
	if err != nil {
		cancel()
		_ = reader.Close()
		streamErr := <-done
		if streamErr != nil {
			if stderr.String() != "" {
				return nil, fmt.Errorf("kubernetes runtime: runtimed tunnel: %w: %s", streamErr, stderr.String())
			}
			return nil, streamErr
		}
		return nil, fmt.Errorf("kubernetes runtime: decode tunneled response: %w", err)
	}
	response.Body = &tunnelResponseBody{ReadCloser: response.Body, pipe: reader, cancel: cancel, done: done}
	return response, nil
}

type tunnelResponseBody struct {
	io.ReadCloser
	pipe   *io.PipeReader
	cancel context.CancelFunc
	done   <-chan error
	once   sync.Once
	err    error
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
	err    error
}

func (b *cancelOnClose) Close() error {
	b.once.Do(func() {
		b.err = b.ReadCloser.Close()
		b.cancel()
	})
	return b.err
}

func (b *tunnelResponseBody) Close() error {
	b.once.Do(func() {
		b.cancel()
		_ = b.ReadCloser.Close()
		_ = b.pipe.Close()
		err := <-b.done
		if err != nil && !errors.Is(err, context.Canceled) {
			b.err = err
		}
	})
	return b.err
}

func (a *Adapter) runningPod(ctx context.Context, meta sandboxMetadata) (*corev1.Pod, error) {
	queryContext, cancel := a.withTimeout(ctx, a.config.Timeouts.API)
	defer cancel()
	selector := managedLabel + "=true," + sandboxIDLabel + "=" + meta.id + "," + componentLabel + "=" + sandboxContainer
	pods, err := a.core.Pods(meta.namespace).List(queryContext, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, mapAPIError("list", "pods/"+meta.namespace, err)
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == sandboxContainer && status.State.Running != nil {
				return pod, nil
			}
		}
	}
	return nil, ErrSandboxNotReady
}

func runtimedResponseError(operation string, response *http.Response) error {
	message, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
	if len(message) == 0 {
		return fmt.Errorf("kubernetes runtime: runtimed %s: %s", operation, response.Status)
	}
	return fmt.Errorf("kubernetes runtime: runtimed %s: %s: %s", operation, response.Status, bytes.TrimSpace(message))
}
