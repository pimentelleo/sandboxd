package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	discoveryv1client "k8s.io/client-go/kubernetes/typed/discovery/v1"
	"k8s.io/client-go/rest"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtimebackend"
)

const (
	managedLabel   = "sandboxd.io/managed"
	sandboxIDLabel = "sandboxd.io/sandbox-id"
	ownerLabel     = "sandboxd.io/owner"
	componentLabel = "sandboxd.io/component"

	deploymentName = "sandbox"
	serviceName    = "preview"
	workspaceName  = "workspace"
	tmpName        = "tmp"
	varTmpName     = "var-tmp"
	quotaName      = "sandboxd-budget"
	limitRangeName = "sandboxd-defaults"

	sandboxContainer              = "sandbox"
	gitOpsContainer               = "git-ops"
	workspacePermissionsContainer = "workspace-permissions"
)

var (
	ErrClientRequired      = errors.New("kubernetes runtime: typed Kubernetes client is required")
	ErrExecutorRequired    = errors.New("kubernetes runtime: remote executor is required")
	ErrSandboxIDRequired   = errors.New("kubernetes runtime: sandbox id is required")
	ErrInvalidSandboxLabel = errors.New("kubernetes runtime: invalid sandbox label")
)

// Client is the typed client subset needed by Adapter. kubernetes.Interface
// and fake.Clientset both implement it, which keeps lifecycle tests off a
// live cluster.
type Client interface {
	CoreV1() corev1client.CoreV1Interface
	AppsV1() appsv1client.AppsV1Interface
	DiscoveryV1() discoveryv1client.DiscoveryV1Interface
}

var _ Client = (kubernetes.Interface)(nil)

// Adapter manages one namespace and its resources per sandbox. Its executor is
// optional only for lifecycle-only tests; task, exec, and file operations
// refuse to run without one.
type Adapter struct {
	config    Config
	core      corev1client.CoreV1Interface
	apps      appsv1client.AppsV1Interface
	discovery discoveryv1client.DiscoveryV1Interface
	executor  RemoteExecutor
}

var (
	_ runtimebackend.Lifecycle              = (*Adapter)(nil)
	_ runtimebackend.NonInteractiveExecutor = (*Adapter)(nil)
	_ runtimebackend.ScopedExecutor         = (*Adapter)(nil)
	_ runtimebackend.TTYExecutor            = (*Adapter)(nil)
	_ runtimebackend.WorkspaceFiles         = (*Adapter)(nil)
	_ runtimebackend.PurgeLifecycle         = (*Adapter)(nil)
	_ runtimebackend.PreviewGateway         = (*Adapter)(nil)
	_ runtimebackend.PreviewReadiness       = (*Adapter)(nil)
)

// NewAdapter constructs an adapter from typed clients. Supplying an executor
// enables in-pod task, command, and workspace-file operations.
func NewAdapter(config Config, client Client, executor ...RemoteExecutor) (*Adapter, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, ErrClientRequired
	}
	adapter := &Adapter{
		config:    config,
		core:      client.CoreV1(),
		apps:      client.AppsV1(),
		discovery: client.DiscoveryV1(),
	}
	if len(executor) > 1 {
		return nil, fmt.Errorf("%w: more than one remote executor", ErrUnsafeSandboxSpec)
	}
	if len(executor) == 1 {
		if executor[0] == nil {
			return nil, ErrExecutorRequired
		}
		adapter.executor = executor[0]
	}
	return adapter, nil
}

// NewInCluster constructs production clients from the mounted service account
// credentials. It does not read kubeconfig files or invoke kubectl.
func NewInCluster(config Config) (*Adapter, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes runtime: build in-cluster config: %w", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("kubernetes runtime: build typed client: %w", err)
	}
	executor, err := NewRemoteCommandExecutor(restConfig, client.CoreV1())
	if err != nil {
		return nil, err
	}
	return NewAdapter(config, client, executor)
}

// Create is idempotent reconciliation. The sandbox image and isolation policy
// come exclusively from Config, never from a caller-supplied container spec.
func (a *Adapter) Create(ctx context.Context, spec runtimebackend.SandboxSpec) (runtimebackend.Sandbox, error) {
	return a.Reconcile(ctx, spec)
}

// Reconcile creates or restores the complete sandbox resource set. Existing
// PVCs are validated rather than replaced because a storage request is
// immutable on most provisioners.
func (a *Adapter) Reconcile(ctx context.Context, spec runtimebackend.SandboxSpec) (runtimebackend.Sandbox, error) {
	meta, err := a.metadataFor(spec)
	if err != nil {
		return runtimebackend.Sandbox{}, err
	}
	ctx, cancel := a.withTimeout(ctx, a.config.Timeouts.API)
	defer cancel()

	if err := a.ensureNamespace(ctx, meta); err != nil {
		return runtimebackend.Sandbox{}, err
	}
	if err := a.ensureQuota(ctx, meta); err != nil {
		return runtimebackend.Sandbox{}, err
	}
	if err := a.ensureLimitRange(ctx, meta); err != nil {
		return runtimebackend.Sandbox{}, err
	}
	if err := a.ensureWorkspacePVC(ctx, meta); err != nil {
		return runtimebackend.Sandbox{}, err
	}
	if err := a.ensureDeployment(ctx, meta); err != nil {
		return runtimebackend.Sandbox{}, err
	}
	if err := a.ensureService(ctx, meta); err != nil {
		return runtimebackend.Sandbox{}, err
	}
	return a.inspect(ctx, meta, false)
}

// Inspect reports the desired and observed Deployment state.
func (a *Adapter) Inspect(ctx context.Context, ref runtimebackend.SandboxRef) (runtimebackend.Sandbox, error) {
	meta, err := a.metadataForRef(ref)
	if err != nil {
		return runtimebackend.Sandbox{}, err
	}
	ctx, cancel := a.withTimeout(ctx, a.config.Timeouts.API)
	defer cancel()
	return a.inspect(ctx, meta, true)
}

// List returns all namespaces owned by this adapter's label convention.
func (a *Adapter) List(ctx context.Context) ([]runtimebackend.Sandbox, error) {
	ctx, cancel := a.withTimeout(ctx, a.config.Timeouts.API)
	defer cancel()
	namespaces, err := a.core.Namespaces().List(ctx, metav1.ListOptions{LabelSelector: managedLabel + "=true"})
	if err != nil {
		return nil, mapAPIError("list", "namespaces", err)
	}
	result := make([]runtimebackend.Sandbox, 0, len(namespaces.Items))
	for _, namespace := range namespaces.Items {
		id := namespace.Labels[sandboxIDLabel]
		if id == "" || !strings.HasPrefix(namespace.Name, a.config.NamespacePrefix+"-") {
			continue
		}
		meta := sandboxMetadata{
			id:        id,
			namespace: namespace.Name,
			labels:    copyLabels(namespace.Labels),
		}
		sandbox, inspectErr := a.inspect(ctx, meta, false)
		if inspectErr != nil {
			return nil, inspectErr
		}
		result = append(result, sandbox)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Ref.ID < result[j].Ref.ID })
	return result, nil
}

// Start reapplies the current workload policy before scaling a retained
// Deployment to one replica. It never recreates or deletes the workspace PVC.
func (a *Adapter) Start(ctx context.Context, ref runtimebackend.SandboxRef) error {
	meta, err := a.metadataForRef(ref)
	if err != nil {
		return err
	}
	ctx, cancel := a.withTimeout(ctx, a.config.Timeouts.API)
	defer cancel()

	deployments := a.apps.Deployments(meta.namespace)
	existing, err := deployments.Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return mapAPIError("get", "deployment/"+meta.namespace+"/"+deploymentName, err)
	}
	if existing.Labels[managedLabel] != "true" || existing.Labels[sandboxIDLabel] != meta.id ||
		existing.Labels[ownerLabel] == "" {
		return fmt.Errorf("%w: deployment/%s/%s has invalid sandbox labels", ErrConflict, meta.namespace, deploymentName)
	}

	// Retain the validated owner and caller labels while replacing the pod
	// template, so a wake also rolls out policy fixes to retained workspaces.
	desired := a.deployment(sandboxMetadata{
		id:        meta.id,
		namespace: meta.namespace,
		labels:    copyLabels(existing.Labels),
	})
	desired.ResourceVersion = existing.ResourceVersion
	replicas := int32(1)
	desired.Spec.Replicas = &replicas
	if _, err := deployments.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
		return mapAPIError("update", "deployment/"+meta.namespace+"/"+deploymentName, err)
	}
	return nil
}

// Stop scales the Deployment to zero replicas and retains every durable
// resource. Kubernetes applies the Deployment termination grace period.
func (a *Adapter) Stop(ctx context.Context, ref runtimebackend.SandboxRef, _ time.Duration) error {
	return a.scale(ctx, ref, 0)
}

// Pause has no Kubernetes equivalent that preserves the Kata workload's
// process state, so it explicitly reports unsupported rather than pretending
// to pause by scaling down.
func (a *Adapter) Pause(context.Context, runtimebackend.SandboxRef) error {
	return ErrUnsupported
}

// Unpause has no Kubernetes equivalent; Start is the intentional wake path.
func (a *Adapter) Unpause(context.Context, runtimebackend.SandboxRef) error {
	return ErrUnsupported
}

// Remove removes only transient workload and service resources. It
// intentionally retains the PVC and namespace; callers must invoke Purge for
// irreversible workspace deletion.
func (a *Adapter) Remove(ctx context.Context, ref runtimebackend.SandboxRef) error {
	meta, err := a.metadataForRef(ref)
	if err != nil {
		return err
	}
	ctx, cancel := a.withTimeout(ctx, a.config.Timeouts.API)
	defer cancel()
	if err := deleteIgnoringNotFound(ctx, a.apps.Deployments(meta.namespace), deploymentName, "delete", "deployment/"+meta.namespace+"/"+deploymentName); err != nil {
		return err
	}
	return deleteIgnoringNotFound(ctx, a.core.Services(meta.namespace), serviceName, "delete", "service/"+meta.namespace+"/"+serviceName)
}

// Purge is the explicit, irreversible removal operation. It deletes the
// workload, Service, PVC, and namespace only when the caller selects purge
// semantics; ordinary Remove never reaches this code path.
func (a *Adapter) Purge(ctx context.Context, ref runtimebackend.SandboxRef) error {
	meta, err := a.metadataForRef(ref)
	if err != nil {
		return err
	}
	ctx, cancel := a.withTimeout(ctx, a.config.Timeouts.API)
	defer cancel()
	if err := a.Remove(ctx, ref); err != nil {
		return err
	}
	if err := deleteIgnoringNotFound(ctx, a.core.PersistentVolumeClaims(meta.namespace), workspaceName, "delete", "pvc/"+meta.namespace+"/"+workspaceName); err != nil {
		return err
	}
	return deleteIgnoringNotFound(ctx, a.core.Namespaces(), meta.namespace, "delete", "namespace/"+meta.namespace)
}

func (a *Adapter) scale(ctx context.Context, ref runtimebackend.SandboxRef, replicas int32) error {
	meta, err := a.metadataForRef(ref)
	if err != nil {
		return err
	}
	ctx, cancel := a.withTimeout(ctx, a.config.Timeouts.API)
	defer cancel()
	deployment, err := a.apps.Deployments(meta.namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return mapAPIError("get", "deployment/"+meta.namespace+"/"+deploymentName, err)
	}
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == replicas {
		return nil
	}
	deployment.Spec.Replicas = &replicas
	if _, err := a.apps.Deployments(meta.namespace).Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
		return mapAPIError("scale", "deployment/"+meta.namespace+"/"+deploymentName, err)
	}
	return nil
}

func (a *Adapter) inspect(ctx context.Context, meta sandboxMetadata, notFoundIsError bool) (runtimebackend.Sandbox, error) {
	deployment, err := a.apps.Deployments(meta.namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && !notFoundIsError {
		return runtimebackend.Sandbox{
			Ref:    runtimebackend.SandboxRef{ID: meta.id, RuntimeID: meta.namespace},
			State:  runtimebackend.LifecycleDeleted,
			Labels: copyLabels(meta.labels),
		}, nil
	}
	if err != nil {
		return runtimebackend.Sandbox{}, mapAPIError("get", "deployment/"+meta.namespace+"/"+deploymentName, err)
	}
	state := runtimebackend.LifecyclePending
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas == 0 {
		state = runtimebackend.LifecycleStopped
	} else if deployment.Status.AvailableReplicas > 0 {
		state = runtimebackend.LifecycleRunning
	} else if deployment.Status.UnavailableReplicas > 0 {
		state = runtimebackend.LifecyclePending
	}
	image := ""
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == sandboxContainer {
			image = container.Image
			break
		}
	}
	return runtimebackend.Sandbox{
		Ref:    runtimebackend.SandboxRef{ID: meta.id, RuntimeID: meta.namespace},
		State:  state,
		Image:  image,
		Labels: copyLabels(meta.labels),
	}, nil
}

type sandboxMetadata struct {
	id        string
	namespace string
	labels    map[string]string
}

func (a *Adapter) metadataFor(spec runtimebackend.SandboxSpec) (sandboxMetadata, error) {
	if len(spec.Env) != 0 || len(spec.Command) != 0 || spec.Image != "" && spec.Image != a.config.SandboxImage {
		return sandboxMetadata{}, fmt.Errorf("%w: image, command, and environment are policy-controlled", ErrUnsafeSandboxSpec)
	}
	labels, err := parseSandboxLabels(spec.Labels)
	if err != nil {
		return sandboxMetadata{}, err
	}
	owner, ok := labels[ownerLabel]
	if !ok || owner == "" {
		return sandboxMetadata{}, ErrOwnerLabelRequired
	}
	id, err := normalizeLabelValue(spec.Ref.ID)
	if err != nil {
		return sandboxMetadata{}, fmt.Errorf("%w: %v", ErrSandboxIDRequired, err)
	}
	owner, err = normalizeLabelValue(owner)
	if err != nil {
		return sandboxMetadata{}, fmt.Errorf("%w: owner: %v", ErrInvalidSandboxLabel, err)
	}
	namespace := a.namespaceForID(id)
	if spec.Ref.RuntimeID != "" && spec.Ref.RuntimeID != namespace {
		return sandboxMetadata{}, ErrRuntimeIDMismatch
	}
	labels[managedLabel] = "true"
	labels[sandboxIDLabel] = id
	labels[ownerLabel] = owner
	return sandboxMetadata{id: id, namespace: namespace, labels: labels}, nil
}

func (a *Adapter) metadataForRef(ref runtimebackend.SandboxRef) (sandboxMetadata, error) {
	id, err := normalizeLabelValue(ref.ID)
	if err != nil {
		return sandboxMetadata{}, fmt.Errorf("%w: %v", ErrSandboxIDRequired, err)
	}
	namespace := a.namespaceForID(id)
	if ref.RuntimeID != "" && ref.RuntimeID != namespace {
		return sandboxMetadata{}, ErrRuntimeIDMismatch
	}
	return sandboxMetadata{id: id, namespace: namespace, labels: map[string]string{
		managedLabel: "true", sandboxIDLabel: id,
	}}, nil
}

func (a *Adapter) namespaceForID(id string) string {
	return boundedDNSLabel(a.config.NamespacePrefix+"-"+id, 63)
}

func parseSandboxLabels(values []string) (map[string]string, error) {
	labels := make(map[string]string, len(values)+3)
	for _, pair := range values {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("%w: %q must be key=value", ErrInvalidSandboxLabel, pair)
		}
		if errs := kvalidation.IsQualifiedName(key); len(errs) > 0 {
			return nil, fmt.Errorf("%w: label key %q: %s", ErrInvalidSandboxLabel, key, strings.Join(errs, "; "))
		}
		// owner values are normalized below so identity strings such as an
		// email address need not already be Kubernetes label values.
		if key != ownerLabel {
			if errs := kvalidation.IsValidLabelValue(value); len(errs) > 0 {
				return nil, fmt.Errorf("%w: label value %q: %s", ErrInvalidSandboxLabel, value, strings.Join(errs, "; "))
			}
		}
		if key == managedLabel || key == sandboxIDLabel || key == componentLabel {
			return nil, fmt.Errorf("%w: label %q is reserved", ErrInvalidSandboxLabel, key)
		}
		labels[key] = value
	}
	return labels, nil
}

func normalizeLabelValue(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", errors.New("empty value")
	}
	var builder strings.Builder
	lastSeparator := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastSeparator = false
		default:
			if !lastSeparator {
				builder.WriteByte('-')
				lastSeparator = true
			}
		}
	}
	normalized := strings.Trim(builder.String(), "-")
	if normalized == "" {
		return "", errors.New("contains no label characters")
	}
	return boundedDNSLabel(normalized, 63), nil
}

func boundedDNSLabel(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	suffix := "-" + hex.EncodeToString(sum[:])[:10]
	return strings.TrimRight(value[:maximum-len(suffix)], "-") + suffix
}

func (a *Adapter) ensureNamespace(ctx context.Context, meta sandboxMetadata) error {
	namespaces := a.core.Namespaces()
	existing, err := namespaces.Get(ctx, meta.namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = namespaces.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: meta.namespace, Labels: copyLabels(meta.labels),
		}}, metav1.CreateOptions{})
		return mapAPIError("create", "namespace/"+meta.namespace, err)
	}
	if err != nil {
		return mapAPIError("get", "namespace/"+meta.namespace, err)
	}
	for key, value := range requiredLabels(meta.labels) {
		if existing.Labels[key] != value {
			return fmt.Errorf("%w: namespace/%s label %q does not match", ErrConflict, meta.namespace, key)
		}
	}
	return nil
}

func (a *Adapter) ensureQuota(ctx context.Context, meta sandboxMetadata) error {
	quotas := a.core.ResourceQuotas(meta.namespace)
	desired := a.resourceQuota(meta)
	existing, err := quotas.Get(ctx, quotaName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = quotas.Create(ctx, desired, metav1.CreateOptions{})
		return mapAPIError("create", "resourcequota/"+meta.namespace+"/"+quotaName, err)
	}
	if err != nil {
		return mapAPIError("get", "resourcequota/"+meta.namespace+"/"+quotaName, err)
	}
	desired.ResourceVersion = existing.ResourceVersion
	_, err = quotas.Update(ctx, desired, metav1.UpdateOptions{})
	return mapAPIError("update", "resourcequota/"+meta.namespace+"/"+quotaName, err)
}

func (a *Adapter) ensureLimitRange(ctx context.Context, meta sandboxMetadata) error {
	ranges := a.core.LimitRanges(meta.namespace)
	desired := a.limitRange(meta)
	existing, err := ranges.Get(ctx, limitRangeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = ranges.Create(ctx, desired, metav1.CreateOptions{})
		return mapAPIError("create", "limitrange/"+meta.namespace+"/"+limitRangeName, err)
	}
	if err != nil {
		return mapAPIError("get", "limitrange/"+meta.namespace+"/"+limitRangeName, err)
	}
	desired.ResourceVersion = existing.ResourceVersion
	_, err = ranges.Update(ctx, desired, metav1.UpdateOptions{})
	return mapAPIError("update", "limitrange/"+meta.namespace+"/"+limitRangeName, err)
}

func (a *Adapter) ensureWorkspacePVC(ctx context.Context, meta sandboxMetadata) error {
	claims := a.core.PersistentVolumeClaims(meta.namespace)
	desired := a.workspacePVC(meta)
	existing, err := claims.Get(ctx, workspaceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = claims.Create(ctx, desired, metav1.CreateOptions{})
		return mapAPIError("create", "pvc/"+meta.namespace+"/"+workspaceName, err)
	}
	if err != nil {
		return mapAPIError("get", "pvc/"+meta.namespace+"/"+workspaceName, err)
	}
	if existing.Spec.StorageClassName == nil || *existing.Spec.StorageClassName != a.config.StorageClass ||
		!hasRWO(existing.Spec.AccessModes) ||
		existing.Spec.Resources.Requests.Storage().Cmp(a.config.Resources.WorkspaceSize) < 0 {
		return fmt.Errorf("%w: existing pvc/%s/%s does not match workspace policy", ErrConflict, meta.namespace, workspaceName)
	}
	return nil
}

func (a *Adapter) ensureDeployment(ctx context.Context, meta sandboxMetadata) error {
	deployments := a.apps.Deployments(meta.namespace)
	desired := a.deployment(meta)
	existing, err := deployments.Get(ctx, deploymentName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = deployments.Create(ctx, desired, metav1.CreateOptions{})
		return mapAPIError("create", "deployment/"+meta.namespace+"/"+deploymentName, err)
	}
	if err != nil {
		return mapAPIError("get", "deployment/"+meta.namespace+"/"+deploymentName, err)
	}
	desired.ResourceVersion = existing.ResourceVersion
	_, err = deployments.Update(ctx, desired, metav1.UpdateOptions{})
	return mapAPIError("update", "deployment/"+meta.namespace+"/"+deploymentName, err)
}

func (a *Adapter) ensureService(ctx context.Context, meta sandboxMetadata) error {
	services := a.core.Services(meta.namespace)
	desired := a.service(meta)
	existing, err := services.Get(ctx, serviceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = services.Create(ctx, desired, metav1.CreateOptions{})
		return mapAPIError("create", "service/"+meta.namespace+"/"+serviceName, err)
	}
	if err != nil {
		return mapAPIError("get", "service/"+meta.namespace+"/"+serviceName, err)
	}
	desired.ResourceVersion = existing.ResourceVersion
	desired.Spec.ClusterIP = existing.Spec.ClusterIP
	desired.Spec.ClusterIPs = append([]string(nil), existing.Spec.ClusterIPs...)
	desired.Spec.IPFamilies = append([]corev1.IPFamily(nil), existing.Spec.IPFamilies...)
	desired.Spec.IPFamilyPolicy = existing.Spec.IPFamilyPolicy
	_, err = services.Update(ctx, desired, metav1.UpdateOptions{})
	return mapAPIError("update", "service/"+meta.namespace+"/"+serviceName, err)
}

func (a *Adapter) resourceQuota(meta sandboxMetadata) *corev1.ResourceQuota {
	requests := addResources(a.config.Resources.Requests, gitOpsRequests())
	limits := addResources(a.config.Resources.Limits, gitOpsLimits())
	hard := corev1.ResourceList{
		corev1.ResourcePods:                   resource.MustParse("1"),
		corev1.ResourcePersistentVolumeClaims: resource.MustParse("1"),
		corev1.ResourceRequestsCPU:            requests[corev1.ResourceCPU],
		corev1.ResourceRequestsMemory:         requests[corev1.ResourceMemory],
		corev1.ResourceLimitsCPU:              limits[corev1.ResourceCPU],
		corev1.ResourceLimitsMemory:           limits[corev1.ResourceMemory],
		corev1.ResourceRequestsStorage:        a.config.Resources.WorkspaceSize,
	}
	return &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: quotaName, Namespace: meta.namespace, Labels: copyLabels(meta.labels)}, Spec: corev1.ResourceQuotaSpec{Hard: hard}}
}

func (a *Adapter) limitRange(meta sandboxMetadata) *corev1.LimitRange {
	return &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: limitRangeName, Namespace: meta.namespace, Labels: copyLabels(meta.labels)},
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
			Type:           corev1.LimitTypeContainer,
			DefaultRequest: copyResources(a.config.Resources.Requests),
			Default:        copyResources(a.config.Resources.Limits),
			Max:            copyResources(a.config.Resources.Limits),
		}}},
	}
}

func (a *Adapter) workspacePVC(meta sandboxMetadata) *corev1.PersistentVolumeClaim {
	class := a.config.StorageClass
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceName, Namespace: meta.namespace, Labels: copyLabels(meta.labels)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: a.config.Resources.WorkspaceSize,
			}},
		},
	}
}

func (a *Adapter) deployment(meta sandboxMetadata) *appsv1.Deployment {
	replicas := int32(1)
	automount := false
	enableServiceLinks := false
	shareProcessNamespace := false
	runAsUser := a.config.Security.RunAsUser
	runAsGroup := a.config.Security.RunAsGroup
	fsGroup := a.config.Security.FSGroup
	nonRoot := a.config.Security.RunAsNonRoot
	readonlyRoot := a.config.Security.ReadOnlyRootFilesystem
	noPrivilegeEscalation := a.config.Security.AllowPrivilegeEscalation
	privileged := false
	seccomp := a.config.Security.SeccompProfile
	selector := map[string]string{managedLabel: "true", sandboxIDLabel: meta.id, componentLabel: sandboxContainer}
	labels := copyLabels(meta.labels)
	labels[componentLabel] = sandboxContainer
	workspaceMount := corev1.VolumeMount{Name: workspaceName, MountPath: a.config.WorkspaceMount}
	temporaryMounts := []corev1.VolumeMount{
		{Name: tmpName, MountPath: "/tmp"},
		{Name: varTmpName, MountPath: "/var/tmp"},
	}
	sandboxEnv := []corev1.EnvVar{}
	if a.config.AgentProxyURL != "" {
		sandboxEnv = append(sandboxEnv, corev1.EnvVar{
			Name:  "RUNTIMED_ANTHROPIC_PROXY",
			Value: a.config.AgentProxyURL,
		})
	}
	podSpec := corev1.PodSpec{
		AutomountServiceAccountToken: &automount,
		EnableServiceLinks:           &enableServiceLinks,
		ShareProcessNamespace:        &shareProcessNamespace,
		HostNetwork:                  false,
		HostPID:                      false,
		HostIPC:                      false,
		NodeSelector:                 copyLabels(a.config.NodeSelector),
		Tolerations:                  append([]corev1.Toleration(nil), a.config.Tolerations...),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser:      &runAsUser,
			RunAsGroup:     &runAsGroup,
			RunAsNonRoot:   &nonRoot,
			FSGroup:        &fsGroup,
			SeccompProfile: &corev1.SeccompProfile{Type: seccomp},
		},
		Volumes: []corev1.Volume{{
			Name: workspaceName,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: workspaceName,
			}},
		}, {
			Name: tmpName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium:    corev1.StorageMediumMemory,
				SizeLimit: resource.NewQuantity(512<<20, resource.BinarySI),
			}},
		}, {
			Name: varTmpName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium:    corev1.StorageMediumMemory,
				SizeLimit: resource.NewQuantity(128<<20, resource.BinarySI),
			}},
		}},
		Containers: []corev1.Container{
			{
				Name:  sandboxContainer,
				Image: a.config.SandboxImage,
				Env:   sandboxEnv,
				Ports: []corev1.ContainerPort{{Name: "web", ContainerPort: a.config.WebPort, Protocol: corev1.ProtocolTCP}},
				Resources: corev1.ResourceRequirements{
					Requests: copyResources(a.config.Resources.Requests),
					Limits:   copyResources(a.config.Resources.Limits),
				},
				VolumeMounts: append([]corev1.VolumeMount{workspaceMount}, temporaryMounts...),
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:                &runAsUser,
					RunAsGroup:               &runAsGroup,
					RunAsNonRoot:             &nonRoot,
					ReadOnlyRootFilesystem:   &readonlyRoot,
					AllowPrivilegeEscalation: &noPrivilegeEscalation,
					Privileged:               &privileged,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: seccomp},
				},
			},
			{
				Name:    gitOpsContainer,
				Image:   a.gitOpsImage(),
				Command: []string{"/bin/sh", "-c", "exec sleep infinity"},
				Resources: corev1.ResourceRequirements{
					Requests: gitOpsRequests(),
					Limits:   gitOpsLimits(),
				},
				VolumeMounts: append([]corev1.VolumeMount{workspaceMount}, temporaryMounts...),
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:                &runAsUser,
					RunAsGroup:               &runAsGroup,
					RunAsNonRoot:             &nonRoot,
					ReadOnlyRootFilesystem:   &readonlyRoot,
					AllowPrivilegeEscalation: &noPrivilegeEscalation,
					Privileged:               &privileged,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: seccomp},
				},
			},
		},
	}
	if a.config.RuntimeClass != "" {
		runtimeClass := a.config.RuntimeClass
		podSpec.RuntimeClassName = &runtimeClass
	}
	if a.config.RuntimeProfile == RuntimeProfileLocalKind {
		initRunAsUser := int64(0)
		initRunAsGroup := int64(0)
		initRunAsNonRoot := false
		workspace := a.config.WorkspaceMount + "/workspace"
		podSpec.InitContainers = []corev1.Container{{
			Name:       workspacePermissionsContainer,
			Image:      a.config.SandboxImage,
			WorkingDir: a.config.WorkspaceMount,
			Command: []string{"/bin/sh", "-ec", fmt.Sprintf(
				`workspace=%q; if [ -L "$workspace" ] || { [ -e "$workspace" ] && [ ! -d "$workspace" ]; }; then echo "workspace must be a directory" >&2; exit 1; fi; mkdir -p "$workspace"; chmod 0770 "$workspace"; chown --no-dereference %d:%d "$workspace"`,
				workspace, runAsUser, runAsGroup,
			)},
			Resources: corev1.ResourceRequirements{
				Requests: gitOpsRequests(),
				Limits:   gitOpsLimits(),
			},
			VolumeMounts: []corev1.VolumeMount{workspaceMount},
			SecurityContext: &corev1.SecurityContext{
				RunAsUser:                &initRunAsUser,
				RunAsGroup:               &initRunAsGroup,
				RunAsNonRoot:             &initRunAsNonRoot,
				ReadOnlyRootFilesystem:   &readonlyRoot,
				AllowPrivilegeEscalation: &noPrivilegeEscalation,
				Privileged:               &privileged,
				Capabilities: &corev1.Capabilities{
					// FOWNER repairs the mode of a retained workspace after it
					// has already been assigned to the unprivileged sandbox user.
					Add:  []corev1.Capability{"CHOWN", "FOWNER"},
					Drop: []corev1.Capability{"ALL"},
				},
				SeccompProfile: &corev1.SeccompProfile{Type: seccomp},
			},
		}}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: deploymentName, Namespace: meta.namespace, Labels: copyLabels(meta.labels)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       podSpec,
			},
		},
	}
}

func (a *Adapter) service(meta sandboxMetadata) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: meta.namespace, Labels: copyLabels(meta.labels)},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{managedLabel: "true", sandboxIDLabel: meta.id, componentLabel: sandboxContainer},
			Ports: []corev1.ServicePort{{
				Name: "web", Port: a.config.WebPort, TargetPort: intstr.FromInt32(a.config.WebPort), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
}

func (a *Adapter) gitOpsImage() string {
	if a.config.GitOpsImage != "" {
		return a.config.GitOpsImage
	}
	return a.config.SandboxImage
}

func (a *Adapter) withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline && time.Until(deadline) <= timeout {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func requiredLabels(labels map[string]string) map[string]string {
	return map[string]string{
		managedLabel: labels[managedLabel], sandboxIDLabel: labels[sandboxIDLabel], ownerLabel: labels[ownerLabel],
	}
}

func copyLabels(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func copyResources(values corev1.ResourceList) corev1.ResourceList {
	result := make(corev1.ResourceList, len(values))
	for key, value := range values {
		result[key] = value.DeepCopy()
	}
	return result
}

func addResources(left, right corev1.ResourceList) corev1.ResourceList {
	result := copyResources(left)
	for key, value := range right {
		if current, found := result[key]; found {
			current.Add(value)
			result[key] = current
		} else {
			result[key] = value.DeepCopy()
		}
	}
	return result
}

func gitOpsRequests() corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}
}

func gitOpsLimits() corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("256Mi")}
}

func hasRWO(modes []corev1.PersistentVolumeAccessMode) bool {
	for _, mode := range modes {
		if mode == corev1.ReadWriteOnce {
			return true
		}
	}
	return false
}

type deleteClient interface {
	Delete(context.Context, string, metav1.DeleteOptions) error
}

func deleteIgnoringNotFound(ctx context.Context, client deleteClient, name, operation, object string) error {
	err := client.Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return mapAPIError(operation, object, err)
}
