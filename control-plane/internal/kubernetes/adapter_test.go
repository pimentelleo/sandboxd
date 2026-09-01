package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/utils/exec"

	runtimed "github.com/tastyeffectco/sandboxd/control-plane/internal/runtime"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtimebackend"
)

func testConfig() Config {
	config := DefaultConfig()
	config.StorageClass = "fast.csi.example"
	config.SandboxImage = "registry.example/sandboxd-base:0.3.0"
	return config
}

func testSpec() runtimebackend.SandboxSpec {
	return runtimebackend.SandboxSpec{
		Ref:    runtimebackend.SandboxRef{ID: "01JY-TEST"},
		Labels: []string{ownerLabel + "=Alice Example <alice@example.test>"},
	}
}

func TestConfigValidatesIsolationAndResourcePolicy(t *testing.T) {
	config := testConfig()
	if err := config.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	config.RuntimeClass = "runc"
	if err := config.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("runtime class error = %v", err)
	}
	config = testConfig()
	config.WorkspaceMount = "/workspace"
	if err := config.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("workspace mount error = %v", err)
	}
	config = testConfig()
	config.WebPort = 4173
	if err := config.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("web port error = %v", err)
	}
	config = testConfig()
	cpu := config.Resources.Limits[corev1.ResourceCPU].DeepCopy()
	cpu.Add(config.Resources.Limits[corev1.ResourceCPU])
	config.Resources.Requests[corev1.ResourceCPU] = cpu
	if err := config.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("resource policy error = %v", err)
	}
}

func TestAdapterReconcilesSecureResourcesAndRetainsPVC(t *testing.T) {
	client := k8sfake.NewSimpleClientset()
	adapter, err := NewAdapter(testConfig(), client)
	if err != nil {
		t.Fatal(err)
	}
	created, err := adapter.Create(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Ref.RuntimeID == "" || created.Ref.RuntimeID == testSpec().Ref.ID {
		t.Fatalf("provider runtime ID = %q", created.Ref.RuntimeID)
	}
	namespace := created.Ref.RuntimeID
	namespaceObject, err := client.CoreV1().Namespaces().Get(context.Background(), namespace, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if namespaceObject.Labels[sandboxIDLabel] != "01jy-test" || namespaceObject.Labels[ownerLabel] != "alice-example-alice-example-test" {
		t.Fatalf("labels were not normalized: %#v", namespaceObject.Labels)
	}
	claim, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(context.Background(), workspaceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasRWO(claim.Spec.AccessModes) || claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "fast.csi.example" {
		t.Fatalf("unsafe PVC: %#v", claim.Spec)
	}
	if _, err := client.CoreV1().ResourceQuotas(namespace).Get(context.Background(), quotaName, metav1.GetOptions{}); err != nil {
		t.Fatalf("resource quota: %v", err)
	}
	if _, err := client.CoreV1().LimitRanges(namespace).Get(context.Background(), limitRangeName, metav1.GetOptions{}); err != nil {
		t.Fatalf("limit range: %v", err)
	}
	deployment, err := client.AppsV1().Deployments(namespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertDeploymentSecure(t, deployment, testConfig())
	service, err := client.CoreV1().Services(namespace).Get(context.Background(), serviceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.Ports[0].Port != DefaultWebPort {
		t.Fatalf("service = %#v", service.Spec)
	}

	if err := adapter.Stop(context.Background(), created.Ref, 0); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	deployment, _ = client.AppsV1().Deployments(namespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
		t.Fatalf("stop replicas = %#v", deployment.Spec.Replicas)
	}
	if err := adapter.Start(context.Background(), created.Ref); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deployment, _ = client.AppsV1().Deployments(namespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
	if *deployment.Spec.Replicas != 1 {
		t.Fatalf("start replicas = %d", *deployment.Spec.Replicas)
	}
	if err := adapter.Remove(context.Background(), created.Ref); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(context.Background(), workspaceName, metav1.GetOptions{}); err != nil {
		t.Fatalf("ordinary remove deleted PVC: %v", err)
	}
	if _, err := client.CoreV1().Namespaces().Get(context.Background(), namespace, metav1.GetOptions{}); err != nil {
		t.Fatalf("ordinary remove deleted namespace: %v", err)
	}
	if err := adapter.Purge(context.Background(), created.Ref); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(context.Background(), workspaceName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("purge retained PVC: %v", err)
	}
}

func TestEnsurePreviewWakesPrivateServiceOnly(t *testing.T) {
	client := k8sfake.NewSimpleClientset()
	config := testConfig()
	adapter, err := NewAdapter(config, client)
	if err != nil {
		t.Fatal(err)
	}
	created, err := adapter.Create(context.Background(), testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Stop(context.Background(), created.Ref, 0); err != nil {
		t.Fatal(err)
	}

	target, err := adapter.EnsurePreview(context.Background(), created.Ref)
	if err != nil {
		t.Fatalf("EnsurePreview: %v", err)
	}
	want := "http://preview." + created.Ref.RuntimeID + ".svc.cluster.local:3000"
	if target.URL != want {
		t.Fatalf("preview target = %q; want %q", target.URL, want)
	}
	deployment, err := client.AppsV1().Deployments(created.Ref.RuntimeID).Get(context.Background(), deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		t.Fatalf("EnsurePreview did not wake sandbox: %#v", deployment.Spec.Replicas)
	}
}

func TestEnsurePreviewUsesConfiguredClusterDomain(t *testing.T) {
	client := k8sfake.NewSimpleClientset()
	config := testConfig()
	config.ClusterDomain = "kubernetes.internal"
	adapter, err := NewAdapter(config, client)
	if err != nil {
		t.Fatal(err)
	}
	created, err := adapter.Create(context.Background(), testSpec())
	if err != nil {
		t.Fatal(err)
	}

	target, err := adapter.EnsurePreview(context.Background(), created.Ref)
	if err != nil {
		t.Fatalf("EnsurePreview: %v", err)
	}
	want := "http://preview." + created.Ref.RuntimeID + ".svc.kubernetes.internal:3000"
	if target.URL != want {
		t.Fatalf("preview target = %q; want %q", target.URL, want)
	}
}

func TestWaitForPreviewReadyRequiresReadyServiceAndHTTPProbe(t *testing.T) {
	tests := []struct {
		name          string
		endpointReady bool
		previewStatus runtimed.PreviewStatus
		previewError  string
		wantErr       bool
	}{
		{name: "ready endpoint and HTTP probe", endpointReady: true, previewStatus: runtimed.PreviewReady},
		{name: "service has no ready endpoint", previewStatus: runtimed.PreviewReady, wantErr: true},
		{
			name:          "development server error",
			endpointReady: true,
			previewStatus: runtimed.PreviewError,
			previewError:  "compile failed",
			wantErr:       true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := previewStatusExecutor(test.previewStatus, test.previewError)
			adapter, ref, client := runningAdapter(t, executor)
			if test.endpointReady {
				createReadyPreviewEndpointSlice(t, client, ref)
			} else {
				adapter.config.Timeouts.Preview = 20 * time.Millisecond
			}

			err := adapter.WaitForPreviewReady(context.Background(), ref)
			if (err != nil) != test.wantErr {
				t.Fatalf("WaitForPreviewReady() error = %v, want error=%t", err, test.wantErr)
			}
			if test.wantErr && !errors.Is(err, ErrPreviewNotReady) {
				t.Fatalf("WaitForPreviewReady() error = %v, want ErrPreviewNotReady", err)
			}
		})
	}
}

func previewStatusExecutor(status runtimed.PreviewStatus, message string) *fakeRemoteExecutor {
	return &fakeRemoteExecutor{stream: func(_ context.Context, request RemoteExecRequest) error {
		raw, err := io.ReadAll(request.Stdin)
		if err != nil {
			return err
		}
		if !bytes.Contains(raw, []byte("GET /status ")) {
			return fmt.Errorf("unexpected request %q", raw)
		}
		payload := fmt.Sprintf(`{"preview":{"status":%q,"build_error_message":%q}}`, status, message)
		_, err = fmt.Fprintf(request.Stdout, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nContent-Type: application/json\r\n\r\n%s", len(payload), payload)
		return err
	}}
}

func createReadyPreviewEndpointSlice(t *testing.T, client *k8sfake.Clientset, ref runtimebackend.SandboxRef) {
	t.Helper()
	ready := true
	_, err := client.DiscoveryV1().EndpointSlices(ref.RuntimeID).Create(context.Background(), &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "preview-ready",
			Namespace: ref.RuntimeID,
			Labels:    map[string]string{discoveryv1.LabelServiceName: serviceName},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.0.0.1"},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create EndpointSlice: %v", err)
	}
}

func assertDeploymentSecure(t *testing.T, deployment *appsv1.Deployment, config Config) {
	t.Helper()
	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("sandbox deployment strategy = %q; want %q", deployment.Spec.Strategy.Type, appsv1.RecreateDeploymentStrategyType)
	}
	pod := deployment.Spec.Template.Spec
	if pod.HostNetwork || pod.HostPID || pod.HostIPC || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		pod.ShareProcessNamespace == nil || *pod.ShareProcessNamespace {
		t.Fatalf("unsafe pod spec: %#v", pod)
	}
	if config.RuntimeProfile == RuntimeProfileLocalKind {
		if pod.RuntimeClassName != nil {
			t.Fatalf("local Kind pod must omit runtime class: %#v", pod.RuntimeClassName)
		}
		if len(pod.InitContainers) != 1 {
			t.Fatalf("local Kind workspace preparation = %#v", pod.InitContainers)
		}
		workspacePermissions := pod.InitContainers[0]
		if workspacePermissions.Name != workspacePermissionsContainer || workspacePermissions.Image != config.SandboxImage ||
			!hasVolumeMount(workspacePermissions.VolumeMounts, workspaceName, DefaultWorkspaceMount) ||
			workspacePermissions.WorkingDir != DefaultWorkspaceMount ||
			len(workspacePermissions.Command) != 3 || !strings.Contains(workspacePermissions.Command[2], "chown --no-dereference") ||
			workspacePermissions.SecurityContext == nil || workspacePermissions.SecurityContext.RunAsUser == nil ||
			*workspacePermissions.SecurityContext.RunAsUser != 0 || workspacePermissions.SecurityContext.RunAsNonRoot == nil ||
			*workspacePermissions.SecurityContext.RunAsNonRoot || workspacePermissions.SecurityContext.Privileged == nil ||
			*workspacePermissions.SecurityContext.Privileged || workspacePermissions.SecurityContext.AllowPrivilegeEscalation == nil ||
			*workspacePermissions.SecurityContext.AllowPrivilegeEscalation || !hasCapabilityDrop(workspacePermissions.SecurityContext.Capabilities, "ALL") ||
			!hasCapabilityAdd(workspacePermissions.SecurityContext.Capabilities, "CHOWN") ||
			!hasCapabilityAdd(workspacePermissions.SecurityContext.Capabilities, "FOWNER") ||
			len(workspacePermissions.SecurityContext.Capabilities.Add) != 2 ||
			workspacePermissions.SecurityContext.ReadOnlyRootFilesystem == nil ||
			!*workspacePermissions.SecurityContext.ReadOnlyRootFilesystem {
			t.Fatalf("unsafe local Kind workspace preparation: %#v", workspacePermissions)
		}
	} else if pod.RuntimeClassName == nil || *pod.RuntimeClassName != DefaultRuntimeClass {
		t.Fatalf("unsafe pod runtime class: %#v", pod.RuntimeClassName)
	} else if len(pod.InitContainers) != 0 {
		t.Fatalf("production deployment has local workspace preparation: %#v", pod.InitContainers)
	}
	for _, volume := range pod.Volumes {
		if volume.HostPath != nil {
			t.Fatalf("host path volume: %#v", volume)
		}
	}
	var sandbox, gitOps *corev1.Container
	for index := range pod.Containers {
		container := &pod.Containers[index]
		switch container.Name {
		case sandboxContainer:
			sandbox = container
		case gitOpsContainer:
			gitOps = container
		}
	}
	if sandbox == nil || gitOps == nil {
		t.Fatalf("containers = %#v", pod.Containers)
	}
	if sandbox.Image != config.SandboxImage || sandbox.SecurityContext == nil || sandbox.SecurityContext.Privileged == nil ||
		*sandbox.SecurityContext.Privileged || sandbox.SecurityContext.AllowPrivilegeEscalation == nil ||
		*sandbox.SecurityContext.AllowPrivilegeEscalation || !hasCapabilityDrop(sandbox.SecurityContext.Capabilities, "ALL") {
		t.Fatalf("unsafe sandbox container: %#v", sandbox)
	}
	if len(gitOps.Env) != 0 || hasCredentialVolume(gitOps.VolumeMounts) {
		t.Fatalf("git sidecar has secret material: %#v", gitOps)
	}
	if sandbox.VolumeMounts[0].MountPath != DefaultWorkspaceMount || gitOps.VolumeMounts[0].MountPath != DefaultWorkspaceMount {
		t.Fatalf("workspace mount mismatch: sandbox=%#v git=%#v", sandbox.VolumeMounts, gitOps.VolumeMounts)
	}
	if !hasMemoryEmptyDir(pod.Volumes, tmpName, "512Mi") || !hasMemoryEmptyDir(pod.Volumes, varTmpName, "128Mi") {
		t.Fatalf("missing bounded temporary volumes: %#v", pod.Volumes)
	}
	for _, container := range []*corev1.Container{sandbox, gitOps} {
		if !hasVolumeMount(container.VolumeMounts, tmpName, "/tmp") ||
			!hasVolumeMount(container.VolumeMounts, varTmpName, "/var/tmp") {
			t.Fatalf("container %q lacks writable temporary mounts: %#v", container.Name, container.VolumeMounts)
		}
	}
}

func TestLocalKindDeploymentOmitsRuntimeClassWithoutWeakeningPodSecurity(t *testing.T) {
	config := DefaultConfig()
	config.StorageClass = "standard"
	config.SandboxImage = "sandboxd-base:kind"
	config.RuntimeProfile = RuntimeProfileLocalKind
	config.RuntimeClass = ""
	adapter, err := NewAdapter(config, k8sfake.NewSimpleClientset())
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	deployment := adapter.deployment(sandboxMetadata{
		id:        "01hlocalkindtest0000000000",
		namespace: "sandboxd-01hlocalkindtest0000000000",
		labels: requiredLabels(map[string]string{
			sandboxIDLabel: "01hlocalkindtest0000000000",
		}),
	})
	assertDeploymentSecure(t, deployment, config)
}

func TestStartReconcilesRetainedLocalKindDeployment(t *testing.T) {
	config := testConfig()
	config.StorageClass = "standard"
	config.RuntimeProfile = RuntimeProfileLocalKind
	config.RuntimeClass = ""
	client := k8sfake.NewSimpleClientset()
	adapter, err := NewAdapter(config, client)
	if err != nil {
		t.Fatal(err)
	}
	created, err := adapter.Create(context.Background(), testSpec())
	if err != nil {
		t.Fatal(err)
	}

	deployments := client.AppsV1().Deployments(created.Ref.RuntimeID)
	stale, err := deployments.Get(context.Background(), deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopped := int32(0)
	stale.Spec.Replicas = &stopped
	stale.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
	stale.Spec.Template.Spec.InitContainers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"CHOWN"}
	if _, err := deployments.Update(context.Background(), stale, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := adapter.Start(context.Background(), created.Ref); err != nil {
		t.Fatalf("Start: %v", err)
	}
	reconciled, err := deployments.Get(context.Background(), deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Spec.Replicas == nil || *reconciled.Spec.Replicas != 1 {
		t.Fatalf("start replicas = %#v", reconciled.Spec.Replicas)
	}
	if reconciled.Labels[ownerLabel] != stale.Labels[ownerLabel] {
		t.Fatalf("start lost owner label: %#v", reconciled.Labels)
	}
	assertDeploymentSecure(t, reconciled, config)
	if _, err := client.CoreV1().PersistentVolumeClaims(created.Ref.RuntimeID).Get(context.Background(), workspaceName, metav1.GetOptions{}); err != nil {
		t.Fatalf("start removed workspace PVC: %v", err)
	}
}

func hasMemoryEmptyDir(volumes []corev1.Volume, name, size string) bool {
	want := resource.MustParse(size)
	for _, volume := range volumes {
		if volume.Name != name || volume.EmptyDir == nil {
			continue
		}
		if volume.EmptyDir.Medium == corev1.StorageMediumMemory &&
			volume.EmptyDir.SizeLimit != nil && volume.EmptyDir.SizeLimit.Cmp(want) == 0 {
			return true
		}
	}
	return false
}

func hasVolumeMount(mounts []corev1.VolumeMount, name, path string) bool {
	for _, mount := range mounts {
		if mount.Name == name && mount.MountPath == path {
			return true
		}
	}
	return false
}

func hasCapabilityDrop(capabilities *corev1.Capabilities, want corev1.Capability) bool {
	if capabilities == nil {
		return false
	}
	for _, got := range capabilities.Drop {
		if got == want {
			return true
		}
	}
	return false
}

func hasCapabilityAdd(capabilities *corev1.Capabilities, want corev1.Capability) bool {
	if capabilities == nil {
		return false
	}
	for _, got := range capabilities.Add {
		if got == want {
			return true
		}
	}
	return false
}

func hasCredentialVolume(mounts []corev1.VolumeMount) bool {
	for _, mount := range mounts {
		if strings.Contains(strings.ToLower(mount.Name), "credential") || strings.Contains(strings.ToLower(mount.Name), "secret") {
			return true
		}
	}
	return false
}

func TestAdapterMapsKubernetesErrors(t *testing.T) {
	client := k8sfake.NewSimpleClientset()
	adapter, err := NewAdapter(testConfig(), client)
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Inspect(context.Background(), runtimebackend.SandboxRef{ID: "missing"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing inspect error = %v", err)
	}
	if _, err := adapter.Create(context.Background(), runtimebackend.SandboxSpec{Ref: runtimebackend.SandboxRef{ID: "id"}}); !errors.Is(err, ErrOwnerLabelRequired) {
		t.Fatalf("missing owner error = %v", err)
	}
}

type fakeRemoteExecutor struct {
	mu       sync.Mutex
	requests []RemoteExecRequest
	stream   func(context.Context, RemoteExecRequest) error
}

func (f *fakeRemoteExecutor) Stream(ctx context.Context, request RemoteExecRequest) error {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	return f.stream(ctx, request)
}

func (f *fakeRemoteExecutor) lastRequest() RemoteExecRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func runningAdapter(t *testing.T, executor *fakeRemoteExecutor) (*Adapter, runtimebackend.SandboxRef, *k8sfake.Clientset) {
	t.Helper()
	client := k8sfake.NewSimpleClientset()
	adapter, err := NewAdapter(testConfig(), client, executor)
	if err != nil {
		t.Fatal(err)
	}
	created, err := adapter.Create(context.Background(), testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Pods(created.Ref.RuntimeID).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "sandbox-pod",
			Labels: map[string]string{managedLabel: "true", sandboxIDLabel: "01jy-test", componentLabel: sandboxContainer},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: sandboxContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return adapter, created.Ref, client
}

func TestTaskRuntimeUsesPrivateTunnelAndStreamsEvents(t *testing.T) {
	executor := &fakeRemoteExecutor{stream: func(_ context.Context, request RemoteExecRequest) error {
		if request.Container != sandboxContainer || request.Command[0] != "runtimed-tunnel" || request.Command[1] != "stdio" {
			return fmt.Errorf("unexpected exec request: %#v", request)
		}
		raw, err := io.ReadAll(request.Stdin)
		if err != nil {
			return err
		}
		if !bytes.Contains(raw, []byte("GET /status ")) && !bytes.Contains(raw, []byte("GET /tasks/task/events?since=7 ")) {
			return fmt.Errorf("unexpected HTTP request %q", raw)
		}
		if bytes.Contains(raw, []byte("/events?")) {
			payload := "{\"id\":8,\"type\":\"done\",\"ts\":\"2026-01-01T00:00:00Z\"}\n"
			_, err = fmt.Fprintf(request.Stdout, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(payload), payload)
		} else {
			payload := "{\"runtimed\":{\"version\":\"test\"},\"preview\":{\"status\":\"ready\"}}"
			_, err = fmt.Fprintf(request.Stdout, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nContent-Type: application/json\r\n\r\n%s", len(payload), payload)
		}
		return err
	}}
	adapter, ref, _ := runningAdapter(t, executor)
	runtime := adapter.BindTaskRuntime(ref)
	status, err := runtime.Status(context.Background())
	if err != nil || status.Preview.Status != runtimed.PreviewReady {
		t.Fatalf("Status = %#v, %v", status, err)
	}
	events, err := runtime.TaskEvents(context.Background(), "task", 7)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	var seen []runtimed.Event
	if err := runtimed.DecodeEvents(events, func(event runtimed.Event) bool {
		seen = append(seen, event)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Type != runtimed.EventDone {
		t.Fatalf("events = %#v", seen)
	}
}

func TestNonInteractiveExecAndTTYResizeObserveCancellation(t *testing.T) {
	sizeReceived := make(chan remotecommand.TerminalSize, 1)
	executor := &fakeRemoteExecutor{stream: func(ctx context.Context, request RemoteExecRequest) error {
		if request.TTY {
			size := request.Resize.Next()
			if size == nil {
				return context.Canceled
			}
			sizeReceived <- *size
			<-ctx.Done()
			return ctx.Err()
		}
		if strings.Join(request.Command, " ") == "false" {
			return utilexec.CodeExitError{Err: errors.New("failed"), Code: 9}
		}
		_, _ = io.WriteString(request.Stdout, "ok")
		return nil
	}}
	adapter, ref, _ := runningAdapter(t, executor)
	result, err := adapter.Exec(context.Background(), ref, runtimebackend.Command{Args: []string{"false"}})
	if err != nil || result.ExitCode != 9 {
		t.Fatalf("Exec = %#v, %v", result, err)
	}
	terminal, err := adapter.OpenTTY(context.Background(), ref, runtimebackend.TTYRequest{Args: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := terminal.Resize(24, 100); err != nil {
		t.Fatal(err)
	}
	select {
	case size := <-sizeReceived:
		if size.Height != 24 || size.Width != 100 {
			t.Fatalf("resize = %#v", size)
		}
	case <-time.After(time.Second):
		t.Fatal("TTY resize was not forwarded")
	}
	if err := terminal.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := terminal.Wait(); err != nil {
		t.Fatalf("TTY wait: %v", err)
	}
}

func TestWorkspaceFilesUseInPodHelperWithBoundedLogicalPath(t *testing.T) {
	executor := &fakeRemoteExecutor{stream: func(_ context.Context, request RemoteExecRequest) error {
		if request.Container != sandboxContainer || len(request.Command) != 4 || request.Command[0] != "runtimed-tunnel" ||
			request.Command[1] != "file" || request.Command[2] != "--root" || request.Command[3] != DefaultWorkspaceMount {
			return fmt.Errorf("unexpected file exec request: %#v", request)
		}
		var input fileRequest
		if err := json.NewDecoder(request.Stdin).Decode(&input); err != nil {
			return err
		}
		if input.Operation != fileRead || input.Path != "workspace/app/main.go" || input.MaxBytes != 32 {
			return fmt.Errorf("unsafe file request: %#v", input)
		}
		return json.NewEncoder(request.Stdout).Encode(fileResponse{Contents: []byte("package main")})
	}}
	adapter, ref, _ := runningAdapter(t, executor)
	logical, err := runtimebackend.ParseLogicalPath("workspace/app/main.go")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := adapter.ReadFile(context.Background(), ref, runtimebackend.ReadFileRequest{Path: logical, MaxBytes: 32})
	if err != nil || string(contents) != "package main" {
		t.Fatalf("ReadFile = %q, %v", contents, err)
	}
	if _, err := runtimebackend.ParseLogicalPath("../../host"); err == nil {
		t.Fatal("unsafe logical path was accepted")
	}
}

func TestGitCredentialStdinIsDirectedOnlyToGitSidecar(t *testing.T) {
	executor := &fakeRemoteExecutor{stream: func(_ context.Context, request RemoteExecRequest) error {
		if request.Container != gitOpsContainer || strings.Join(request.Command, " ") != "git fetch origin main" {
			return fmt.Errorf("unexpected git exec request: %#v", request)
		}
		credential, err := io.ReadAll(request.Stdin)
		if err != nil {
			return err
		}
		if string(credential) != "protocol=https\nhost=git.example\n" {
			return fmt.Errorf("unexpected credential stdio payload")
		}
		_, err = io.WriteString(request.Stdout, "ok")
		return err
	}}
	adapter, ref, _ := runningAdapter(t, executor)
	result, err := adapter.ExecGit(context.Background(), ref, GitRequest{
		Operation: GitFetch,
		Args:      []string{"origin", "main"},
		Stdin:     strings.NewReader("protocol=https\nhost=git.example\n"),
	})
	if err != nil || result.Stdout != "ok" {
		t.Fatalf("ExecGit = %#v, %v", result, err)
	}
}
