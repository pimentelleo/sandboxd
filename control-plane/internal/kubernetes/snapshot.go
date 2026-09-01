package kubernetes

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

var volumeSnapshotGVR = schema.GroupVersionResource{
	Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots",
}

// VolumeSnapshotRef identifies a CSI snapshot and its source PVC without
// exposing unstructured API details to callers.
type VolumeSnapshotRef struct {
	Namespace string
	Name      string
	SourcePVC string
}

// RestorePVCReference is the typed PVC data-source reference required when a
// later snapshot API integration creates a restored workspace claim.
type RestorePVCReference struct {
	Snapshot VolumeSnapshotRef
}

func (r RestorePVCReference) DataSource() *corev1.TypedLocalObjectReference {
	apiGroup := volumeSnapshotGVR.Group
	return &corev1.TypedLocalObjectReference{
		APIGroup: &apiGroup,
		Kind:     "VolumeSnapshot",
		Name:     r.Snapshot.Name,
	}
}

// SnapshotRequest describes a CSI VolumeSnapshot create operation.
type SnapshotRequest struct {
	Name      string
	SourcePVC string
	ClassName string
}

// SnapshotHelper is a typed boundary around the optional CSI snapshot CRD.
// DynamicClient is necessary because VolumeSnapshot is not part of core
// client-go API groups.
type SnapshotHelper struct {
	client    dynamic.Interface
	discovery discovery.DiscoveryInterface
}

// NewSnapshotHelper creates a snapshot helper with an injectable dynamic
// client for tests. A discovery client is optional but recommended in
// production so the CSI CRD can be verified before operations are attempted.
func NewSnapshotHelper(client dynamic.Interface, discoveryClient ...discovery.DiscoveryInterface) (*SnapshotHelper, error) {
	if client == nil {
		return nil, ErrClientRequired
	}
	if len(discoveryClient) > 1 {
		return nil, fmt.Errorf("%w: more than one discovery client", ErrUnsafeSandboxSpec)
	}
	helper := &SnapshotHelper{client: client}
	if len(discoveryClient) == 1 {
		if discoveryClient[0] == nil {
			return nil, ErrClientRequired
		}
		helper.discovery = discoveryClient[0]
	}
	return helper, nil
}

func (h *SnapshotHelper) Create(ctx context.Context, namespace string, request SnapshotRequest) (VolumeSnapshotRef, error) {
	if namespace == "" || request.Name == "" || request.SourcePVC == "" || request.ClassName == "" {
		return VolumeSnapshotRef{}, fmt.Errorf("%w: snapshot namespace, name, source PVC, and class are required", ErrUnsafeSandboxSpec)
	}
	if err := h.Supported(); err != nil {
		return VolumeSnapshotRef{}, err
	}
	object := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata": map[string]interface{}{
			"name":      request.Name,
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"volumeSnapshotClassName": request.ClassName,
			"source": map[string]interface{}{
				"persistentVolumeClaimName": request.SourcePVC,
			},
		},
	}}
	_, err := h.client.Resource(volumeSnapshotGVR).Namespace(namespace).Create(ctx, object, metav1.CreateOptions{})
	if err != nil {
		return VolumeSnapshotRef{}, mapSnapshotError("create", namespace+"/"+request.Name, err)
	}
	return VolumeSnapshotRef{Namespace: namespace, Name: request.Name, SourcePVC: request.SourcePVC}, nil
}

func (h *SnapshotHelper) Get(ctx context.Context, reference VolumeSnapshotRef) (*unstructured.Unstructured, error) {
	if reference.Namespace == "" || reference.Name == "" {
		return nil, fmt.Errorf("%w: snapshot namespace and name are required", ErrUnsafeSandboxSpec)
	}
	if err := h.Supported(); err != nil {
		return nil, err
	}
	object, err := h.client.Resource(volumeSnapshotGVR).Namespace(reference.Namespace).Get(ctx, reference.Name, metav1.GetOptions{})
	if err != nil {
		return nil, mapSnapshotError("get", reference.Namespace+"/"+reference.Name, err)
	}
	return object, nil
}

func (h *SnapshotHelper) Delete(ctx context.Context, reference VolumeSnapshotRef) error {
	if reference.Namespace == "" || reference.Name == "" {
		return fmt.Errorf("%w: snapshot namespace and name are required", ErrUnsafeSandboxSpec)
	}
	if err := h.Supported(); err != nil {
		return err
	}
	err := h.client.Resource(volumeSnapshotGVR).Namespace(reference.Namespace).Delete(ctx, reference.Name, metav1.DeleteOptions{})
	if err != nil {
		return mapSnapshotError("delete", reference.Namespace+"/"+reference.Name, err)
	}
	return nil
}

// Supported explicitly verifies the CSI snapshot CRD when a discovery client
// was supplied. Dynamic-only construction remains useful for focused tests and
// still maps a create endpoint 404 to the same actionable error.
func (h *SnapshotHelper) Supported() error {
	if h.discovery == nil {
		return nil
	}
	resources, err := h.discovery.ServerResourcesForGroupVersion(volumeSnapshotGVR.GroupVersion().String())
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: %v", ErrVolumeSnapshotMissing, err)
		}
		return mapAPIError("discover", "volumesnapshot API", err)
	}
	if resources == nil {
		return fmt.Errorf("%w: discovery returned no resources", ErrVolumeSnapshotMissing)
	}
	for _, apiResource := range resources.APIResources {
		if apiResource.Name == volumeSnapshotGVR.Resource && apiResource.Namespaced {
			return nil
		}
	}
	return fmt.Errorf("%w: resource %q is absent", ErrVolumeSnapshotMissing, volumeSnapshotGVR.Resource)
}

func mapSnapshotError(operation, object string, err error) error {
	// POST cannot target a missing snapshot, so an endpoint 404 without
	// discovery is an actionable indication that the snapshot CRD is absent.
	if operation == "create" && apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %v", ErrVolumeSnapshotMissing, err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return mapAPIError(operation, "volumesnapshot/"+object, err)
}
