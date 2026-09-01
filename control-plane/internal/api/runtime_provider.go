package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	runtimed "github.com/tastyeffectco/sandboxd/control-plane/internal/runtime"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtimebackend"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

var (
	// ErrRuntimeOperationBusy is intentionally distinct from provider failures:
	// another control-plane replica owns the durable lifecycle fence.
	ErrRuntimeOperationBusy = errors.New("runtime operation already in progress")
	ErrRuntimeUnavailable   = errors.New("runtime provider is unavailable")
	// ErrRuntimeStarting tells callers that Kubernetes has accepted a start but
	// the sandbox has not yet reached a state where its private transports can
	// be used. It is retryable and deliberately distinct from a provider error.
	ErrRuntimeStarting = errors.New("runtime provider sandbox is still starting")
)

const (
	providerStartupWait     = 30 * time.Second
	providerStartupPollRate = 250 * time.Millisecond
)

func (s *Server) usesRuntimeProvider() bool {
	return s.RuntimeLifecycle != nil
}

func (s *Server) runtimeRef(sb *store.Sandbox) runtimebackend.SandboxRef {
	if sb == nil {
		return runtimebackend.SandboxRef{}
	}
	return runtimebackend.SandboxRef{ID: sb.ID, RuntimeID: sb.ContainerID.String}
}

func (s *Server) runtimeLeaseTTL() time.Duration {
	if s.RuntimeLeaseTTL >= 15*time.Second {
		return s.RuntimeLeaseTTL
	}
	return time.Minute
}

func (s *Server) runtimeLeaseHolder() string {
	if s.RuntimeLeaseHolder != "" {
		return s.RuntimeLeaseHolder
	}
	// This fallback exists for single-process tests only. Production startup
	// requires an explicitly unique pod identity before enabling this path.
	return "sandboxd-single-process"
}

// withRuntimeLease fences a provider lifecycle operation across control-plane
// replicas. The heartbeat is active for the entire provider call; callers do
// not claim coordination merely by having a lease table configured.
func (s *Server) withRuntimeLease(ctx context.Context, sandboxID string, fn func(context.Context) error) error {
	return s.withProviderLease(ctx, store.LeaseResourceSandbox, sandboxID,
		func(operationCtx context.Context, _ store.OperationLease) error {
			return fn(operationCtx)
		})
}

// withTaskWatchLease fences one runtimed event watcher across Kubernetes
// replicas. A task watcher must not share a sandbox lifecycle lease because it
// can intentionally outlive short lifecycle operations.
func (s *Server) withTaskWatchLease(ctx context.Context, taskID string, fn func(context.Context, store.OperationLease) error) error {
	if !s.usesRuntimeProvider() || s.Store == nil {
		return ErrRuntimeUnavailable
	}
	return s.withProviderLease(ctx, store.LeaseResourceTask, taskID, fn)
}

// withProviderLease holds a database-backed provider-work lease and passes its
// fenced identity to the callback. Long-running callbacks must use that lease
// when they persist work that could otherwise be completed by a stale replica.
func (s *Server) withProviderLease(ctx context.Context, resource store.LeaseResource, resourceID string, fn func(context.Context, store.OperationLease) error) error {
	if !s.usesRuntimeProvider() {
		return fn(ctx, store.OperationLease{})
	}
	if s.Store == nil {
		return ErrRuntimeUnavailable
	}
	ttl := s.runtimeLeaseTTL()
	lease, err := s.Store.AcquireOperationLease(ctx, resource, resourceID, s.runtimeLeaseHolder(), ttl)
	if errors.Is(err, store.ErrLeaseHeld) {
		return ErrRuntimeOperationBusy
	}
	if err != nil {
		return fmt.Errorf("acquire provider operation lease: %w", err)
	}

	operationCtx, cancel := context.WithCancel(ctx)
	stopHeartbeat := make(chan struct{})
	heartbeatDone := make(chan struct{})
	heartbeatErr := make(chan error, 1)
	interval := ttl / 3
	if interval < time.Second {
		interval = time.Second
	}
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				return
			case <-operationCtx.Done():
				return
			case <-ticker.C:
				if _, err := s.Store.HeartbeatOperationLease(operationCtx, *lease, ttl); err != nil {
					select {
					case heartbeatErr <- err:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()

	runErr := fn(operationCtx, *lease)
	cancel()
	close(stopHeartbeat)
	<-heartbeatDone
	releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
	releaseErr := s.Store.ReleaseOperationLease(releaseCtx, *lease)
	releaseCancel()

	select {
	case err := <-heartbeatErr:
		if runErr == nil {
			runErr = fmt.Errorf("runtime operation lease lost: %w", err)
		}
	default:
	}
	if runErr != nil {
		return runErr
	}
	if releaseErr != nil && !errors.Is(releaseErr, store.ErrLeaseLost) {
		return fmt.Errorf("release provider operation lease: %w", releaseErr)
	}
	return nil
}

func runtimeStatus(state runtimebackend.LifecycleState) string {
	switch state {
	case runtimebackend.LifecycleRunning:
		return "running"
	case runtimebackend.LifecycleStopped, runtimebackend.LifecyclePaused:
		return "stopped"
	case runtimebackend.LifecyclePending:
		return "creating"
	case runtimebackend.LifecycleFailed, runtimebackend.LifecycleDeleted, runtimebackend.LifecycleUnknown:
		return "error"
	default:
		return "error"
	}
}

func (s *Server) persistRuntimeState(ctx context.Context, id string, runtime runtimebackend.Sandbox) error {
	runtimeID := runtime.Ref.RuntimeID
	if runtimeID == "" {
		return errors.New("runtime provider returned no runtime ID")
	}
	status := runtimeStatus(runtime.State)
	if status == "error" {
		return s.Store.MarkError(ctx, id, "runtime provider reported an unavailable sandbox")
	}
	if err := s.Store.SetRuntimeState(ctx, id, status, runtimeID); err != nil {
		return err
	}
	if status == "stopped" {
		return s.Store.MarkStoppedAt(ctx, id, time.Now().UTC())
	}
	return nil
}

// waitForProviderRunning waits a bounded amount of time after a provider wake
// before using an in-pod transport. Each state refresh that can mutate the
// durable row is fenced by the operation lease; another replica's in-flight
// refresh is simply observed on the next poll.
func (s *Server) waitForProviderRunning(ctx context.Context, sandboxID string) (*store.Sandbox, error) {
	if !s.usesRuntimeProvider() || s.Store == nil {
		return nil, ErrRuntimeUnavailable
	}
	waitCtx, cancel := context.WithTimeout(ctx, providerStartupWait)
	defer cancel()
	ticker := time.NewTicker(providerStartupPollRate)
	defer ticker.Stop()

	for {
		sb, err := s.Store.Get(waitCtx, sandboxID)
		if err != nil {
			if waitErr := providerStartupWaitError(ctx, waitCtx); waitErr != nil {
				return nil, waitErr
			}
			return nil, err
		}
		switch sb.Status {
		case "running":
			return sb, nil
		case "error":
			return nil, fmt.Errorf("%w: sandbox is in an error state", ErrRuntimeUnavailable)
		case "stopped":
			return nil, fmt.Errorf("%w: sandbox stopped while starting", ErrRuntimeUnavailable)
		case "creating":
			err = s.withRuntimeLease(waitCtx, sandboxID, func(operationCtx context.Context) error {
				// Re-read after acquiring the durable fence. The row observed
				// before acquiring it may belong to an operation that completed
				// on another control-plane replica while this caller waited.
				current, getErr := s.Store.Get(operationCtx, sandboxID)
				if getErr != nil {
					return getErr
				}
				if current.Status != "creating" {
					return nil
				}
				live, inspectErr := s.RuntimeLifecycle.Inspect(operationCtx, s.runtimeRef(current))
				if inspectErr != nil {
					return inspectErr
				}
				return s.persistRuntimeState(operationCtx, sandboxID, live)
			})
			if err != nil && !errors.Is(err, ErrRuntimeOperationBusy) {
				if waitErr := providerStartupWaitError(ctx, waitCtx); waitErr != nil {
					return nil, waitErr
				}
				return nil, err
			}
		default:
			return nil, fmt.Errorf("%w: sandbox has unsupported state %q", ErrRuntimeUnavailable, sb.Status)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-waitCtx.Done():
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return nil, ErrRuntimeStarting
			}
			return nil, waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func providerStartupWaitError(requestCtx, waitCtx context.Context) error {
	if err := requestCtx.Err(); err != nil {
		return err
	}
	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		return ErrRuntimeStarting
	}
	return nil
}

// ReconcileProvider refreshes durable rows from a provider without touching
// Docker, loopback mounts, or host paths. A lifecycle lease is taken for every
// state mutation so multiple control-plane replicas do not race reconciliation.
func (s *Server) ReconcileProvider(ctx context.Context) {
	if !s.usesRuntimeProvider() || s.Store == nil {
		return
	}
	rows, err := s.Store.List(ctx)
	if err != nil {
		s.Log.Warn("provider reconcile: list sandboxes failed", "err", err.Error())
		return
	}
	for _, listed := range rows {
		sandboxID := listed.ID
		err := s.withRuntimeLease(ctx, sandboxID, func(operationCtx context.Context) error {
			// The list is only a candidate set. Re-fetching while fenced avoids
			// overwriting a concurrent lifecycle transition with stale metadata.
			current, getErr := s.Store.Get(operationCtx, sandboxID)
			if errors.Is(getErr, store.ErrNotFound) {
				return nil
			}
			if getErr != nil {
				return getErr
			}
			if current.Status == "error" {
				return nil
			}
			live, err := s.RuntimeLifecycle.Inspect(operationCtx, s.runtimeRef(current))
			if err != nil {
				return err
			}
			return s.persistRuntimeState(operationCtx, sandboxID, live)
		})
		if err != nil && !errors.Is(err, ErrRuntimeOperationBusy) {
			s.Log.Warn("provider reconcile: inspect failed", "sandbox_id", sandboxID, "err", err.Error())
		}
	}
}

// ReapProviderIdle stops only provider sandboxes that remain idle after
// rechecking their durable state inside the lifecycle lease. Kubernetes owns
// node pressure, so the Docker host-pressure reaper is intentionally not used.
func (s *Server) ReapProviderIdle(ctx context.Context, threshold time.Duration) {
	if !s.usesRuntimeProvider() || s.Store == nil || threshold <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-threshold)
	candidates, err := s.Store.ListIdleCandidates(ctx, cutoff)
	if err != nil {
		s.Log.Warn("provider idle reaper: list candidates failed", "err", err.Error())
		return
	}
	for _, candidate := range candidates {
		if s.Inflight != nil && s.Inflight.Active(candidate.ID) {
			continue
		}
		stopped := false
		err := s.withRuntimeLease(ctx, candidate.ID, func(operationCtx context.Context) error {
			current, err := s.Store.Get(operationCtx, candidate.ID)
			if err != nil {
				return err
			}
			if current.Status != "running" || current.LastActiveAt.After(cutoff) {
				return nil
			}
			running, err := s.Store.SandboxHasRunningTask(operationCtx, candidate.ID)
			if err != nil {
				return err
			}
			if running || (s.Inflight != nil && s.Inflight.Active(candidate.ID)) {
				return nil
			}
			if err := s.RuntimeLifecycle.Stop(operationCtx, s.runtimeRef(current), 0); err != nil {
				return err
			}
			if err := s.persistRuntimeState(operationCtx, candidate.ID, runtimebackend.Sandbox{
				Ref: s.runtimeRef(current), State: runtimebackend.LifecycleStopped,
			}); err != nil {
				return err
			}
			stopped = true
			return nil
		})
		if err != nil && !errors.Is(err, ErrRuntimeOperationBusy) {
			s.Log.Warn("provider idle reaper: stop failed", "sandbox_id", candidate.ID, "err", err.Error())
			continue
		}
		if stopped {
			s.Log.Info("provider idle reaper: stopped sandbox", "sandbox_id", candidate.ID)
		}
	}
}

func (s *Server) providerTaskRuntime(sb *store.Sandbox) (runtimebackend.TaskRuntime, error) {
	if s.TaskRuntime == nil {
		return nil, ErrRuntimeUnavailable
	}
	if sb == nil || sb.ID == "" {
		return nil, errors.New("sandbox is required")
	}
	return s.TaskRuntime.BindTaskRuntime(s.runtimeRef(sb)), nil
}

// providerSandboxStateError identifies a row that changed state after an API
// handler's optimistic read but before it acquired the lifecycle fence.
type providerSandboxStateError struct {
	status string
}

func (e *providerSandboxStateError) Error() string {
	return "sandbox is " + e.status
}

// startProviderTask admits a task durably before sending it across the private
// runtimed transport. The lifecycle lease prevents a concurrent stop or reaper
// from observing an unprotected interval between task admission and start.
func (s *Server) startProviderTask(ctx context.Context, sandboxID string, task *store.Task, request runtimed.StartTaskRequest) (*store.Sandbox, error) {
	if task == nil {
		return nil, errors.New("task is required")
	}

	var started *store.Sandbox
	err := s.withRuntimeLease(ctx, sandboxID, func(operationCtx context.Context) error {
		current, err := s.Store.Get(operationCtx, sandboxID)
		if err != nil {
			return err
		}
		if current.Status != "running" {
			return &providerSandboxStateError{status: current.Status}
		}
		taskRuntime, err := s.providerTaskRuntime(current)
		if err != nil {
			return err
		}

		// Bind attribution to the fenced, current row rather than the earlier
		// optimistic read made while validating the request.
		task.ExternalUserID = current.ExternalUserID
		task.ExternalProjectID = current.ExternalProjectID
		if err := s.Store.CreateTask(operationCtx, task); err != nil {
			return fmt.Errorf("persist task admission: %w", err)
		}
		if err := taskRuntime.StartTask(operationCtx, request); err != nil {
			if finishErr := s.finishProviderTaskStartFailure(task); finishErr != nil {
				return fmt.Errorf("%w; persist rejected task: %v", err, finishErr)
			}
			return err
		}
		started = current
		return nil
	})
	return started, err
}

// finishProviderTaskStartFailure leaves no active durable task after runtimed
// rejects the request. Use a detached bounded context so request cancellation
// cannot strand a running row and block future lifecycle operations.
func (s *Server) finishProviderTaskStartFailure(task *store.Task) error {
	result := failedResult(task.TaskID, "sandbox_unavailable",
		"task could not be started by the runtime provider")
	result.Prompt = task.Prompt
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.Store.FinishTask(ctx, task.TaskID, string(result.Status), string(raw))
}

// withProviderTaskRuntime serializes a short task control operation with
// provider lifecycle changes. It intentionally does not cover task event
// streams, which can be long-lived and are protected from reaping by the
// durable active-task row.
func (s *Server) withProviderTaskRuntime(ctx context.Context, sandboxID string, operation func(context.Context, runtimebackend.TaskRuntime) error) error {
	return s.withRuntimeLease(ctx, sandboxID, func(operationCtx context.Context) error {
		current, err := s.Store.Get(operationCtx, sandboxID)
		if err != nil {
			return err
		}
		if current.Status != "running" {
			return &providerSandboxStateError{status: current.Status}
		}
		taskRuntime, err := s.providerTaskRuntime(current)
		if err != nil {
			return err
		}
		return operation(operationCtx, taskRuntime)
	})
}

// unavailableTaskRuntime makes a missing provider transport fail at the API
// boundary instead of letting provider mode fall back to a host Unix socket.
type unavailableTaskRuntime struct{ err error }

var _ runtimebackend.TaskRuntime = unavailableTaskRuntime{}

func (r unavailableTaskRuntime) Status(context.Context) (*runtimed.Status, error) {
	return nil, r.err
}

func (r unavailableTaskRuntime) StartTask(context.Context, runtimed.StartTaskRequest) error {
	return r.err
}

func (r unavailableTaskRuntime) PrepareHostedTask(context.Context, runtimed.PrepareHostedTaskRequest) (*runtimed.HostedTaskPreparation, error) {
	return nil, r.err
}

func (r unavailableTaskRuntime) FinalizeHostedTask(context.Context, runtimed.FinalizeHostedTaskRequest) (*runtimed.TaskResult, error) {
	return nil, r.err
}

func (r unavailableTaskRuntime) AbandonHostedTask(context.Context, runtimed.AbandonHostedTaskRequest) (*runtimed.TaskResult, error) {
	return nil, r.err
}

func (r unavailableTaskRuntime) CancelTask(context.Context, string) error {
	return r.err
}

func (r unavailableTaskRuntime) RevertTask(context.Context, string) error {
	return r.err
}

func (r unavailableTaskRuntime) TaskEvents(context.Context, string, int) (io.ReadCloser, error) {
	return nil, r.err
}

func writeRuntimeProviderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrRuntimeOperationBusy):
		writeErr(w, 409, "sandbox lifecycle operation is already in progress")
	case errors.Is(err, ErrRuntimeUnavailable):
		writeErr(w, 503, "runtime provider is unavailable")
	default:
		writeErr(w, 502, "runtime provider operation failed: "+err.Error())
	}
}
