package kubernetes

import (
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

var (
	ErrNotFound              = errors.New("kubernetes runtime: resource not found")
	ErrForbidden             = errors.New("kubernetes runtime: access forbidden")
	ErrConflict              = errors.New("kubernetes runtime: resource conflict")
	ErrUnavailable           = errors.New("kubernetes runtime: api unavailable")
	ErrUnsupported           = errors.New("kubernetes runtime: operation unsupported")
	ErrSandboxNotReady       = errors.New("kubernetes runtime: sandbox pod is not ready")
	ErrPreviewNotReady       = errors.New("kubernetes runtime: preview endpoint is not ready")
	ErrRuntimeIDMismatch     = errors.New("kubernetes runtime: runtime id does not match sandbox id")
	ErrOwnerLabelRequired    = errors.New("kubernetes runtime: owner label is required")
	ErrUnsafeSandboxSpec     = errors.New("kubernetes runtime: unsafe sandbox specification")
	ErrOutputLimitExceeded   = errors.New("kubernetes runtime: command output limit exceeded")
	ErrVolumeSnapshotMissing = errors.New("kubernetes runtime: VolumeSnapshot CRD is not installed")
)

// APIError retains the Kubernetes operation and resource while allowing callers
// to use errors.Is for a stable provider-neutral category.
type APIError struct {
	Operation string
	Resource  string
	Err       error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kubernetes runtime: %s %s: %v", e.Operation, e.Resource, e.Err)
}

func (e *APIError) Unwrap() error { return e.Err }

func mapAPIError(operation, object string, err error) error {
	if err == nil {
		return nil
	}
	category := err
	switch {
	case apierrors.IsNotFound(err):
		category = ErrNotFound
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		category = ErrForbidden
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		category = ErrConflict
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsServiceUnavailable(err), apierrors.IsTooManyRequests(err):
		category = ErrUnavailable
	}
	if category == err {
		return &APIError{Operation: operation, Resource: object, Err: err}
	}
	return &APIError{Operation: operation, Resource: object, Err: fmt.Errorf("%w: %v", category, err)}
}
