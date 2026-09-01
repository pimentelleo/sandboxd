package runtimebackend

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
	runtimed "github.com/tastyeffectco/sandboxd/control-plane/internal/runtime"
)

type fakeDocker struct {
	runSpec    docker.RunSpec
	target     string
	stopAfter  int
	ttyContext context.Context
}

func (f *fakeDocker) Run(_ context.Context, spec docker.RunSpec) (string, error) {
	f.runSpec = spec
	return "container-id", nil
}
func (f *fakeDocker) Inspect(_ context.Context, name string) (*docker.ContainerJSON, error) {
	f.target = name
	container := &docker.ContainerJSON{ID: "container-id"}
	container.State.Running = true
	container.State.Pid = 42
	container.Config.Image = "base:latest"
	container.Config.Labels = map[string]string{"managed": "true"}
	return container, nil
}
func (f *fakeDocker) Remove(_ context.Context, name string) error { f.target = name; return nil }
func (f *fakeDocker) Start(_ context.Context, name string) error  { f.target = name; return nil }
func (f *fakeDocker) Stop(_ context.Context, name string, seconds int) error {
	f.target, f.stopAfter = name, seconds
	return nil
}
func (f *fakeDocker) Pause(_ context.Context, name string) error   { f.target = name; return nil }
func (f *fakeDocker) Unpause(_ context.Context, name string) error { f.target = name; return nil }
func (f *fakeDocker) Exec(_ context.Context, name string, _ []string) (docker.ExecResult, error) {
	f.target = name
	return docker.ExecResult{Stdout: "ok", ExitCode: 3}, nil
}
func (f *fakeDocker) ExecScoped(_ context.Context, request docker.ScopedExecRequest) (docker.ExecResult, error) {
	f.target = request.Container
	return docker.ExecResult{Stderr: "bounded", ExitCode: 7}, nil
}
func (f *fakeDocker) ExecTTYContext(ctx context.Context, _ string, _ string, _ string, _ []string) (*os.File, *exec.Cmd, error) {
	f.ttyContext = ctx
	return nil, nil, errors.New("not used in this test")
}

var _ DockerClient = (*fakeDocker)(nil)

func TestDockerAdapterMapsLifecycleAndNonInteractiveExec(t *testing.T) {
	client := &fakeDocker{}
	adapter, err := NewDockerAdapter(client)
	if err != nil {
		t.Fatalf("NewDockerAdapter: %v", err)
	}
	ref := SandboxRef{ID: "sandbox-1", RuntimeID: "docker-name"}
	created, err := adapter.Create(context.Background(), SandboxSpec{
		Ref: ref, RuntimeName: "docker-name", Image: "base:latest", Labels: []string{"managed=true"}, Command: []string{"sleep", "infinity"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Ref.RuntimeID != "container-id" || client.runSpec.Name != "docker-name" || client.runSpec.Cmd[0] != "sleep" {
		t.Fatalf("Create mapping mismatch: sandbox=%+v spec=%+v", created, client.runSpec)
	}
	inspected, err := adapter.Inspect(context.Background(), ref)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inspected.State != LifecycleRunning || inspected.ProcessID != 42 || inspected.Labels["managed"] != "true" {
		t.Fatalf("Inspect mapping mismatch: %+v", inspected)
	}
	result, err := adapter.Exec(context.Background(), ref, Command{Args: []string{"echo", "ok"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "ok" || result.ExitCode != 3 || client.target != "docker-name" {
		t.Fatalf("Exec mapping mismatch: result=%+v target=%q", result, client.target)
	}
	if err := adapter.Stop(context.Background(), ref, 500*time.Millisecond); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if client.stopAfter != 1 {
		t.Fatalf("Stop grace = %d, want 1", client.stopAfter)
	}
}

func TestDockerAdapterDoesNotUseLogicalIDAsCreateTarget(t *testing.T) {
	client := &fakeDocker{}
	adapter, err := NewDockerAdapter(client)
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Create(context.Background(), SandboxSpec{Ref: SandboxRef{ID: "logical-id"}, Image: "base:latest"})
	if !errors.Is(err, ErrRuntimeNameRequired) {
		t.Fatalf("Create without provider target error = %v", err)
	}
	_, err = adapter.Create(context.Background(), SandboxSpec{
		Ref: SandboxRef{ID: "logical-id"}, RuntimeName: "s-derived-docker-name", Image: "base:latest",
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.runSpec.Name != "s-derived-docker-name" {
		t.Fatalf("Docker create target = %q, want provider-selected name", client.runSpec.Name)
	}
}

func TestDockerAdapterPassesTerminalContext(t *testing.T) {
	client := &fakeDocker{}
	adapter, err := NewDockerAdapter(client)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = adapter.OpenTTY(ctx, SandboxRef{ID: "sandbox-1", RuntimeID: "docker-name"}, TTYRequest{Args: []string{"sh"}})
	if err == nil || client.ttyContext != ctx {
		t.Fatalf("OpenTTY error/context = %v/%v", err, client.ttyContext == ctx)
	}
}

func TestDockerLifecycleMapping(t *testing.T) {
	cases := []struct {
		status string
		want   LifecycleState
	}{
		{status: "created", want: LifecyclePending},
		{status: "exited", want: LifecycleStopped},
		{status: "paused", want: LifecyclePaused},
		{status: "removing", want: LifecycleDeleted},
		{status: "unexpected", want: LifecycleUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			container := &docker.ContainerJSON{}
			container.State.Status = tc.status
			if got := dockerLifecycle(container); got != tc.want {
				t.Fatalf("dockerLifecycle(%q) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestAdaptersRejectMissingDependenciesAndReferences(t *testing.T) {
	if _, err := NewDockerAdapter(nil); !errors.Is(err, ErrDockerClientRequired) {
		t.Fatalf("nil Docker adapter error = %v", err)
	}
	if _, err := NewWorkspaceAdapter(nil); !errors.Is(err, ErrWorkspaceManagerRequired) {
		t.Fatalf("nil workspace adapter error = %v", err)
	}
	if _, err := NewUnixRuntimeAdapter(nil); !errors.Is(err, ErrRuntimeClientRequired) {
		t.Fatalf("nil runtime adapter error = %v", err)
	}
	adapter, err := NewDockerAdapter(&fakeDocker{})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Start(context.Background(), SandboxRef{}); !errors.Is(err, ErrRuntimeIDRequired) {
		t.Fatalf("missing ref error = %v", err)
	}
}

type fakeWorkspace struct {
	id   string
	root string
}

func (f *fakeWorkspace) Paths(id string) (string, string) {
	if f.root != "" {
		return f.root, f.root
	}
	return "/data/" + id, "/mnt/" + id
}
func (f *fakeWorkspace) Provision(_ context.Context, id string) error {
	f.id = id
	return nil
}
func (f *fakeWorkspace) ProvisionFromTemplate(_ context.Context, id, _ string) error {
	f.id = id
	return nil
}
func (f *fakeWorkspace) Release(_ context.Context, id string) error { f.id = id; return nil }
func (f *fakeWorkspace) ImgExists(id string) bool                   { return id == "sandbox-1" }
func (f *fakeWorkspace) NormalizeOwnership(string) error            { return nil }

var _ LoopbackManager = (*fakeWorkspace)(nil)

func TestWorkspaceAdapterUsesStableSandboxID(t *testing.T) {
	workspace := &fakeWorkspace{}
	adapter, err := NewWorkspaceAdapter(workspace)
	if err != nil {
		t.Fatal(err)
	}
	ref := SandboxRef{ID: "sandbox-1", RuntimeID: "provider-handle"}
	paths, err := adapter.Paths(ref)
	if err != nil {
		t.Fatal(err)
	}
	if paths.Storage != "/data/sandbox-1" || paths.Mount != "/mnt/sandbox-1" {
		t.Fatalf("paths = %+v", paths)
	}
	if err := adapter.Provision(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if workspace.id != "sandbox-1" {
		t.Fatalf("workspace id = %q", workspace.id)
	}
	exists, err := adapter.Exists(ref)
	if err != nil || !exists {
		t.Fatalf("Exists = %v, %v", exists, err)
	}
}

type fakeRuntimeClient struct{}

func (fakeRuntimeClient) Status(context.Context) (*runtimed.Status, error) {
	return &runtimed.Status{}, nil
}
func (fakeRuntimeClient) StartTask(context.Context, runtimed.StartTaskRequest) error { return nil }
func (fakeRuntimeClient) PrepareHostedTask(context.Context, runtimed.PrepareHostedTaskRequest) (*runtimed.HostedTaskPreparation, error) {
	return &runtimed.HostedTaskPreparation{}, nil
}
func (fakeRuntimeClient) FinalizeHostedTask(context.Context, runtimed.FinalizeHostedTaskRequest) (*runtimed.TaskResult, error) {
	return &runtimed.TaskResult{}, nil
}
func (fakeRuntimeClient) AbandonHostedTask(context.Context, runtimed.AbandonHostedTaskRequest) (*runtimed.TaskResult, error) {
	return &runtimed.TaskResult{}, nil
}
func (fakeRuntimeClient) CancelTask(context.Context, string) error { return nil }
func (fakeRuntimeClient) RevertTask(context.Context, string) error { return nil }
func (fakeRuntimeClient) TaskEvents(context.Context, string, int) (io.ReadCloser, error) {
	return io.NopCloser(nilReader{}), nil
}

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) { return 0, io.EOF }

var _ UnixRuntimeClient = fakeRuntimeClient{}

func TestUnixRuntimeAdapterForwardsStatus(t *testing.T) {
	adapter, err := NewUnixRuntimeAdapter(fakeRuntimeClient{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Status(context.Background()); err != nil {
		t.Fatalf("Status: %v", err)
	}
}
