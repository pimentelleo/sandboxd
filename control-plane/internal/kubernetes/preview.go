package kubernetes

import (
	"context"
	"fmt"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	runtimed "github.com/tastyeffectco/sandboxd/control-plane/internal/runtime"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtimebackend"
)

const previewReadyPollInterval = 250 * time.Millisecond

// EnsurePreview wakes the provider workload before exposing only its cluster-
// internal Service address to the control-plane preview gateway.
func (a *Adapter) EnsurePreview(ctx context.Context, ref runtimebackend.SandboxRef) (runtimebackend.PreviewTarget, error) {
	meta, err := a.metadataForRef(ref)
	if err != nil {
		return runtimebackend.PreviewTarget{}, err
	}
	if err := a.Start(ctx, ref); err != nil {
		return runtimebackend.PreviewTarget{}, err
	}
	return runtimebackend.PreviewTarget{
		URL: fmt.Sprintf("http://%s.%s.svc.%s:%d", serviceName, meta.namespace, a.config.ClusterDomain, a.config.WebPort),
	}, nil
}

// WaitForPreviewReady waits for both the Kubernetes Service endpoint and
// runtimed's HTTP health observation. Pod readiness intentionally does not
// depend on the application port: task and file transports must remain usable
// while a user starts or repairs a development server.
func (a *Adapter) WaitForPreviewReady(ctx context.Context, ref runtimebackend.SandboxRef) error {
	meta, err := a.metadataForRef(ref)
	if err != nil {
		return err
	}
	waitCtx, cancel := a.withTimeout(ctx, a.config.Timeouts.Preview)
	defer cancel()
	ticker := time.NewTicker(previewReadyPollInterval)
	defer ticker.Stop()

	var (
		endpointReady bool
		lastStatus    runtimed.PreviewState
		lastStatusErr error
	)
	for {
		endpointReady, err = a.previewServiceHasReadyEndpoint(waitCtx, meta)
		if err != nil {
			return err
		}
		status, statusErr := a.StatusForSandbox(waitCtx, ref)
		if statusErr == nil {
			lastStatus = status.Preview
			lastStatusErr = nil
			switch status.Preview.Status {
			case runtimed.PreviewReady:
				if endpointReady {
					return nil
				}
			case runtimed.PreviewNone:
				return fmt.Errorf("%w: sandbox has no web process", ErrPreviewNotReady)
			case runtimed.PreviewError:
				if status.Preview.BuildErrorMessage != "" {
					return fmt.Errorf("%w: %s", ErrPreviewNotReady, status.Preview.BuildErrorMessage)
				}
				return fmt.Errorf("%w: development server reported an error", ErrPreviewNotReady)
			}
		} else {
			// The pod can become available before runtimed has bound its private
			// socket. Retrying remains fail-closed because nothing is proxied
			// until its HTTP probe confirms the app is ready.
			lastStatusErr = statusErr
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-waitCtx.Done():
			if err := ctx.Err(); err != nil {
				return err
			}
			if lastStatusErr != nil {
				return fmt.Errorf("%w: private status endpoint unavailable: %v", ErrPreviewNotReady, lastStatusErr)
			}
			if !endpointReady {
				return fmt.Errorf("%w: preview Service has no ready endpoint", ErrPreviewNotReady)
			}
			return fmt.Errorf("%w: development server status is %q", ErrPreviewNotReady, lastStatus.Status)
		case <-ticker.C:
		}
	}
}

func (a *Adapter) previewServiceHasReadyEndpoint(ctx context.Context, meta sandboxMetadata) (bool, error) {
	if a.discovery == nil {
		return false, ErrClientRequired
	}
	queryCtx, cancel := a.withTimeout(ctx, a.config.Timeouts.API)
	defer cancel()
	slices, err := a.discovery.EndpointSlices(meta.namespace).List(queryCtx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + serviceName,
	})
	if err != nil {
		return false, mapAPIError("list", "endpointslices/"+meta.namespace, err)
	}
	for _, slice := range slices.Items {
		for _, endpoint := range slice.Endpoints {
			if endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready ||
				endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating ||
				len(endpoint.Addresses) == 0 {
				continue
			}
			return true, nil
		}
	}
	return false, nil
}
