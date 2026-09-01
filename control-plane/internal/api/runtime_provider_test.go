package api

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	runtimed "github.com/tastyeffectco/sandboxd/control-plane/internal/runtime"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtimebackend"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// providerRuntimeFake implements every provider path exercised by the API tests.
// The test server deliberately leaves Docker and Loopback nil: any accidental
// local-runtime fallback panics instead of silently passing.
type providerRuntimeFake struct {
	mu sync.Mutex

	sandboxes map[string]runtimebackend.Sandbox
	files     map[string][]byte

	createSpecs []runtimebackend.SandboxSpec
	startRefs   []runtimebackend.SandboxRef
	stopRefs    []runtimebackend.SandboxRef
	purgeRefs   []runtimebackend.SandboxRef
	inspectRefs []runtimebackend.SandboxRef
	execRefs    []runtimebackend.SandboxRef
	execCmds    []runtimebackend.Command
	listReqs    []runtimebackend.ListFilesRequest
	readReqs    []runtimebackend.ReadFileRequest
	writeReqs   []runtimebackend.WriteFileRequest
	taskRefs    []runtimebackend.SandboxRef
	taskReqs    []runtimed.StartTaskRequest
	previewRefs []runtimebackend.SandboxRef

	listResult     runtimebackend.ListFilesResult
	execResult     runtimebackend.CommandResult
	startTaskHook  func(runtimed.StartTaskRequest)
	startTaskErr   error
	previewErr     error
	taskEventsHook func(context.Context, string, int) (io.ReadCloser, error)
	taskWatchDone  chan struct{}
}

func newProviderRuntimeFake() *providerRuntimeFake {
	return &providerRuntimeFake{
		sandboxes:     make(map[string]runtimebackend.Sandbox),
		files:         make(map[string][]byte),
		execResult:    runtimebackend.CommandResult{Stdout: "provider output", ExitCode: 7},
		taskWatchDone: make(chan struct{}),
	}
}

func (f *providerRuntimeFake) Create(_ context.Context, spec runtimebackend.SandboxSpec) (runtimebackend.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createSpecs = append(f.createSpecs, spec)
	sb := runtimebackend.Sandbox{
		Ref:   runtimebackend.SandboxRef{ID: spec.Ref.ID, RuntimeID: "sandbox-ns/" + spec.Ref.ID},
		State: runtimebackend.LifecycleRunning,
	}
	f.sandboxes[spec.Ref.ID] = sb
	return sb, nil
}

func (f *providerRuntimeFake) Inspect(_ context.Context, ref runtimebackend.SandboxRef) (runtimebackend.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspectRefs = append(f.inspectRefs, ref)
	sb, ok := f.sandboxes[ref.ID]
	if !ok {
		return runtimebackend.Sandbox{}, errors.New("provider sandbox not found")
	}
	if ref.RuntimeID != "" {
		sb.Ref = ref
	}
	return sb, nil
}

func (f *providerRuntimeFake) Start(_ context.Context, ref runtimebackend.SandboxRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startRefs = append(f.startRefs, ref)
	sb, ok := f.sandboxes[ref.ID]
	if !ok {
		sb = runtimebackend.Sandbox{Ref: ref}
	}
	sb.Ref = ref
	sb.State = runtimebackend.LifecycleRunning
	f.sandboxes[ref.ID] = sb
	return nil
}

func (f *providerRuntimeFake) Stop(_ context.Context, ref runtimebackend.SandboxRef, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopRefs = append(f.stopRefs, ref)
	sb, ok := f.sandboxes[ref.ID]
	if !ok {
		sb = runtimebackend.Sandbox{Ref: ref}
	}
	sb.Ref = ref
	sb.State = runtimebackend.LifecycleStopped
	f.sandboxes[ref.ID] = sb
	return nil
}

func (f *providerRuntimeFake) Pause(context.Context, runtimebackend.SandboxRef) error   { return nil }
func (f *providerRuntimeFake) Unpause(context.Context, runtimebackend.SandboxRef) error { return nil }

func (f *providerRuntimeFake) Remove(_ context.Context, ref runtimebackend.SandboxRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sandboxes, ref.ID)
	return nil
}

func (f *providerRuntimeFake) Purge(_ context.Context, ref runtimebackend.SandboxRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purgeRefs = append(f.purgeRefs, ref)
	delete(f.sandboxes, ref.ID)
	return nil
}

func (f *providerRuntimeFake) ListFiles(_ context.Context, _ runtimebackend.SandboxRef, request runtimebackend.ListFilesRequest) (runtimebackend.ListFilesResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listReqs = append(f.listReqs, request)
	return f.listResult, nil
}

func (f *providerRuntimeFake) ReadFile(_ context.Context, _ runtimebackend.SandboxRef, request runtimebackend.ReadFileRequest) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readReqs = append(f.readReqs, request)
	contents, ok := f.files[request.Path.String()]
	if !ok {
		return nil, runtimebackend.ErrFileNotFound
	}
	return append([]byte(nil), contents...), nil
}

func (f *providerRuntimeFake) WriteFile(_ context.Context, _ runtimebackend.SandboxRef, request runtimebackend.WriteFileRequest) (runtimebackend.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeReqs = append(f.writeReqs, request)
	f.files[request.Path.String()] = append([]byte(nil), request.Contents...)
	return runtimebackend.FileInfo{Path: request.Path, Type: runtimebackend.FileTypeRegular, Size: int64(len(request.Contents))}, nil
}

func (f *providerRuntimeFake) DeleteFile(context.Context, runtimebackend.SandboxRef, runtimebackend.LogicalPath) error {
	return nil
}

func (f *providerRuntimeFake) WaitForPreviewReady(_ context.Context, ref runtimebackend.SandboxRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.previewRefs = append(f.previewRefs, ref)
	return f.previewErr
}

func (f *providerRuntimeFake) Exec(_ context.Context, ref runtimebackend.SandboxRef, command runtimebackend.Command) (runtimebackend.CommandResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execRefs = append(f.execRefs, ref)
	f.execCmds = append(f.execCmds, command)
	return f.execResult, nil
}

func (f *providerRuntimeFake) BindTaskRuntime(ref runtimebackend.SandboxRef) runtimebackend.TaskRuntime {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taskRefs = append(f.taskRefs, ref)
	return f
}

func (f *providerRuntimeFake) Status(context.Context) (*runtimed.Status, error) {
	return &runtimed.Status{}, nil
}

func (f *providerRuntimeFake) StartTask(_ context.Context, request runtimed.StartTaskRequest) error {
	f.mu.Lock()
	f.taskReqs = append(f.taskReqs, request)
	hook, err := f.startTaskHook, f.startTaskErr
	f.mu.Unlock()
	if hook != nil {
		hook(request)
	}
	return err
}

func (f *providerRuntimeFake) PrepareHostedTask(context.Context, runtimed.PrepareHostedTaskRequest) (*runtimed.HostedTaskPreparation, error) {
	return nil, errors.New("not used")
}

func (f *providerRuntimeFake) FinalizeHostedTask(context.Context, runtimed.FinalizeHostedTaskRequest) (*runtimed.TaskResult, error) {
	return nil, errors.New("not used")
}

func (f *providerRuntimeFake) AbandonHostedTask(context.Context, runtimed.AbandonHostedTaskRequest) (*runtimed.TaskResult, error) {
	return nil, errors.New("not used")
}

func (f *providerRuntimeFake) CancelTask(context.Context, string) error { return nil }
func (f *providerRuntimeFake) RevertTask(context.Context, string) error { return nil }

func (f *providerRuntimeFake) TaskEvents(ctx context.Context, taskID string, since int) (io.ReadCloser, error) {
	f.mu.Lock()
	hook := f.taskEventsHook
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, taskID, since)
	}
	return &providerTaskEventStream{Reader: strings.NewReader(""), done: f.taskWatchDone}, nil
}

type providerTaskEventStream struct {
	io.Reader
	done chan struct{}
	once sync.Once
}

func (s *providerTaskEventStream) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

func newProviderAPIServer(t *testing.T) (*Server, *providerRuntimeFake) {
	t.Helper()
	st, err := store.Open(context.Background(), "file::memory:?_fk=1", "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runtime := newProviderRuntimeFake()
	return &Server{
		Store:                 st,
		Log:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Image:                 "sandboxd:test",
		RuntimeLifecycle:      runtime,
		RuntimePurger:         runtime,
		RuntimeFiles:          runtime,
		RuntimeExec:           runtime,
		TaskRuntime:           runtime,
		PreviewReadiness:      runtime,
		ProviderWebPort:       3000,
		ProviderFileByteLimit: 64,
	}, runtime
}

func createProviderSandbox(t *testing.T, s *Server, status string, appID string) *store.Sandbox {
	t.Helper()
	id := newULID()
	runtimeID := "sandbox-ns/" + id
	sb := &store.Sandbox{
		ID:             id,
		Status:         status,
		Image:          "sandboxd:test",
		ExternalUserID: nullStr("tenant-a"),
		AppID:          nullStr(appID),
		Ports:          []int{3000},
		WebPort:        storeNullInt64(3000),
		Visibility:     "private",
		IdlePolicy:     "sleep",
	}
	if err := s.Store.Create(context.Background(), sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if err := s.Store.SetRuntimeState(context.Background(), id, status, runtimeID); err != nil {
		t.Fatalf("set provider runtime state: %v", err)
	}
	fresh, err := s.Store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get provider sandbox: %v", err)
	}
	return fresh
}

func storeNullInt64(value int64) (out sql.NullInt64) {
	return sql.NullInt64{Int64: value, Valid: true}
}

func providerRequest(method, target, id, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.SetPathValue("id", id)
	return r
}

func TestProviderLifecycleRoutesWithoutLocalRuntime(t *testing.T) {
	s, provider := newProviderAPIServer(t)
	id := newULID()
	create := httptest.NewRecorder()
	s.handleCreate(create, httptest.NewRequest(http.MethodPost, "/sandbox",
		strings.NewReader(`{"id":"`+id+`","ports":[3000]}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("provider create = %d: %s", create.Code, create.Body.String())
	}
	if len(provider.createSpecs) != 1 || provider.createSpecs[0].Ref.ID != id {
		t.Fatalf("provider create was not called with sandbox id: %#v", provider.createSpecs)
	}
	created, err := s.Store.Get(context.Background(), id)
	if err != nil || created.Status != "running" || !created.ContainerID.Valid {
		t.Fatalf("provider create state = %#v, %v", created, err)
	}

	exec := httptest.NewRecorder()
	s.handleExec(exec, providerRequest(http.MethodPost, "/sandbox/"+id+"/exec", id,
		`{"cmd":["sh","-lc","echo provider"]}`))
	if exec.Code != http.StatusOK || !strings.Contains(exec.Body.String(), "provider output") {
		t.Fatalf("provider exec = %d: %s", exec.Code, exec.Body.String())
	}
	if len(provider.execRefs) != 1 || provider.execRefs[0].RuntimeID != created.ContainerID.String {
		t.Fatalf("provider exec did not use persisted runtime reference: %#v", provider.execRefs)
	}

	stop := httptest.NewRecorder()
	s.v1StopSandbox(stop, providerRequest(http.MethodPost, "/v1/sandboxes/"+id+"/stop", id, ""))
	if stop.Code != http.StatusOK || len(provider.stopRefs) != 1 {
		t.Fatalf("provider stop = %d, calls=%d: %s", stop.Code, len(provider.stopRefs), stop.Body.String())
	}
	stopped, err := s.Store.Get(context.Background(), id)
	if err != nil || stopped.Status != "stopped" {
		t.Fatalf("provider stop state = %#v, %v", stopped, err)
	}

	start := httptest.NewRecorder()
	s.v1StartSandbox(start, providerRequest(http.MethodPost, "/v1/sandboxes/"+id+"/start", id, ""))
	if start.Code != http.StatusOK || len(provider.startRefs) != 1 {
		t.Fatalf("provider start = %d, calls=%d: %s", start.Code, len(provider.startRefs), start.Body.String())
	}
	running, err := s.Store.Get(context.Background(), id)
	if err != nil || running.Status != "running" {
		t.Fatalf("provider wake state = %#v, %v", running, err)
	}

	deleteResponse := httptest.NewRecorder()
	s.v1DeleteSandbox(deleteResponse, providerRequest(http.MethodDelete, "/v1/sandboxes/"+id, id, ""))
	if deleteResponse.Code != http.StatusNoContent || len(provider.purgeRefs) != 1 {
		t.Fatalf("provider purge = %d, calls=%d: %s", deleteResponse.Code, len(provider.purgeRefs), deleteResponse.Body.String())
	}
	if _, err := s.Store.Get(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("provider purge left sandbox row: %v", err)
	}
}

func TestProviderStartRecoversCreatingSandbox(t *testing.T) {
	s, provider := newProviderAPIServer(t)
	creating := createProviderSandbox(t, s, "creating", "app-1")
	provider.mu.Lock()
	provider.sandboxes[creating.ID] = runtimebackend.Sandbox{
		Ref:   s.runtimeRef(creating),
		State: runtimebackend.LifecyclePending,
	}
	provider.mu.Unlock()

	response := httptest.NewRecorder()
	s.v1StartSandbox(response, providerRequest(http.MethodPost, "/v1/sandboxes/"+creating.ID+"/start", creating.ID, ""))
	if response.Code != http.StatusOK {
		t.Fatalf("provider recovery start = %d: %s", response.Code, response.Body.String())
	}
	if len(provider.startRefs) != 1 || provider.startRefs[0] != s.runtimeRef(creating) {
		t.Fatalf("provider recovery start refs = %#v", provider.startRefs)
	}
	recovered, err := s.Store.Get(context.Background(), creating.ID)
	if err != nil || recovered.Status != "running" {
		t.Fatalf("provider recovery state = %#v, %v", recovered, err)
	}
}

func TestProviderCreateDefaultsPrivateAndRejectsHostCapabilities(t *testing.T) {
	s, provider := newProviderAPIServer(t)
	id := newULID()
	create := httptest.NewRecorder()
	s.handleCreate(create, httptest.NewRequest(http.MethodPost, "/sandbox",
		strings.NewReader(`{"id":"`+id+`","ports":[3000]}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("provider default create = %d: %s", create.Code, create.Body.String())
	}
	created, err := s.Store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if created.Visibility != "private" {
		t.Fatalf("provider default visibility = %q; want private", created.Visibility)
	}
	if len(provider.createSpecs) != 1 {
		t.Fatalf("provider create calls = %d; want 1", len(provider.createSpecs))
	}

	for name, body := range map[string]string{
		"public visibility": `{"id":"` + newULID() + `","visibility":"public"}`,
		"memory high":       `{"id":"` + newULID() + `","memory_high":"2G"}`,
		"template":          `{"id":"` + newULID() + `","template":"react-standard"}`,
		"runtime preset":    `{"id":"` + newULID() + `","runtime_preset":"react-vite"}`,
		"environment":       `{"id":"` + newULID() + `","env":{"EXAMPLE":"value"}}`,
		"git import":        `{"id":"` + newULID() + `","git":{"repo_url":"https://example.test/repo.git"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			rejected := httptest.NewRecorder()
			s.handleCreate(rejected, httptest.NewRequest(http.MethodPost, "/sandbox", strings.NewReader(body)))
			if rejected.Code != http.StatusNotImplemented {
				t.Fatalf("provider %s = %d: %s", name, rejected.Code, rejected.Body.String())
			}
		})
	}
	if len(provider.createSpecs) != 1 {
		t.Fatalf("unsupported requests reached provider create: %#v", provider.createSpecs)
	}
}

func TestProviderV1CreateDefaultsPrivateAndRelaysUnsupported(t *testing.T) {
	s, _ := newProviderAPIServer(t)
	create := httptest.NewRecorder()
	s.v1CreateSandbox(create, reqAs(http.MethodPost, "/v1/sandboxes",
		`{"project":{"id":"project-default","user_id":"tenant-a"}}`, "tenant-a"))
	if create.Code != http.StatusCreated {
		t.Fatalf("provider v1 default create = %d: %s", create.Code, create.Body.String())
	}
	rows, err := s.Store.ListFiltered(context.Background(), "", "project-default")
	if err != nil || len(rows) != 1 {
		t.Fatalf("provider v1 default rows = %#v, %v", rows, err)
	}
	if rows[0].Visibility != "private" {
		t.Fatalf("provider v1 default visibility = %q; want private", rows[0].Visibility)
	}

	unsupported := httptest.NewRecorder()
	s.v1CreateSandbox(unsupported, reqAs(http.MethodPost, "/v1/sandboxes",
		`{"project":{"id":"project-public","user_id":"tenant-a"},"visibility":"public"}`, "tenant-a"))
	if unsupported.Code != http.StatusNotImplemented || !strings.Contains(unsupported.Body.String(), `"code":"unsupported"`) {
		t.Fatalf("provider v1 public create = %d: %s", unsupported.Code, unsupported.Body.String())
	}
}

func TestProviderAppConfigurationRejectsHostBackedSetup(t *testing.T) {
	for name, body := range map[string]string{
		"runtime preset": `{"name":"provider app","runtime_preset":"unknown-preset"}`,
		"git import":     `{"name":"provider app","git":{"repo_url":"https://example.test/repo.git"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newProviderAPIServer(t)
			response := httptest.NewRecorder()
			s.v1CreateApp(response, reqAs(http.MethodPost, "/v1/apps", body, "tenant-a"))
			if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), `"code":"unsupported"`) {
				t.Fatalf("provider app %s = %d: %s", name, response.Code, response.Body.String())
			}
		})
	}

	for name, app := range map[string]*store.App{
		"request template": {
			ID: newULID(), OwnerToken: "tenant-a", Name: "provider app",
		},
		"stored runtime preset": {
			ID: newULID(), OwnerToken: "tenant-a", Name: "provider app",
			RuntimePreset: nullStr("react-vite"),
		},
		"stored git import": {
			ID: newULID(), OwnerToken: "tenant-a", Name: "provider app",
			GitRepoURL: nullStr("https://example.test/repo.git"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, provider := newProviderAPIServer(t)
			if err := s.Store.CreateApp(context.Background(), app); err != nil {
				t.Fatal(err)
			}
			body := ""
			if name == "request template" {
				body = `{"template":"react-standard"}`
			}
			request := reqAs(http.MethodPost, "/v1/apps/"+app.ID+"/sandbox", body, "tenant-a")
			request.SetPathValue("id", app.ID)
			response := httptest.NewRecorder()
			s.v1CreateAppSandbox(response, request)
			if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), `"code":"unsupported"`) {
				t.Fatalf("provider app sandbox %s = %d: %s", name, response.Code, response.Body.String())
			}
			if len(provider.createSpecs) != 0 {
				t.Fatalf("provider app sandbox %s reached runtime create", name)
			}
		})
	}
}

func TestProviderExecAvailableAtVersionedRoute(t *testing.T) {
	s, provider := newProviderAPIServer(t)
	sb := createProviderSandbox(t, s, "running", "")
	request := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/exec",
		strings.NewReader(`{"cmd":["sh","-lc","echo provider"]}`))
	response := httptest.NewRecorder()

	s.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "provider output") {
		t.Fatalf("versioned provider exec = %d: %s", response.Code, response.Body.String())
	}
	if len(provider.execRefs) != 1 || provider.execRefs[0].RuntimeID != sb.ContainerID.String {
		t.Fatalf("versioned provider exec did not use persisted runtime reference: %#v", provider.execRefs)
	}
}

func TestProviderFilesUseProviderLimitsAndNoHostPaths(t *testing.T) {
	s, provider := newProviderAPIServer(t)
	sb := createProviderSandbox(t, s, "running", "")
	path, _ := runtimebackend.ParseLogicalPath("workspace/app/hello.txt")
	provider.files[path.String()] = []byte("hello")
	provider.listResult = runtimebackend.ListFilesResult{
		Entries: []runtimebackend.FileInfo{{Path: path, Type: runtimebackend.FileTypeRegular, Size: 5}},
	}

	content := httptest.NewRecorder()
	s.v1FileContent(content, providerRequest(http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files/content?path=hello.txt", sb.ID, ""))
	if content.Code != http.StatusOK || content.Body.String() != "hello" {
		t.Fatalf("provider file content = %d: %s", content.Code, content.Body.String())
	}
	list := httptest.NewRecorder()
	s.v1ListFiles(list, providerRequest(http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files", sb.ID, ""))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "hello.txt") {
		t.Fatalf("provider file list = %d: %s", list.Code, list.Body.String())
	}
	put := httptest.NewRecorder()
	s.v1PutFile(put, providerRequest(http.MethodPut, "/v1/sandboxes/"+sb.ID+"/files?path=hello.txt", sb.ID, "updated"))
	if put.Code != http.StatusOK {
		t.Fatalf("provider file write = %d: %s", put.Code, put.Body.String())
	}
	manifestPut := httptest.NewRecorder()
	s.v1PutFile(manifestPut, providerRequest(http.MethodPut,
		"/v1/sandboxes/"+sb.ID+"/files?path=sandbox.yaml", sb.ID, "version: 1\n"))
	if manifestPut.Code != http.StatusOK {
		t.Fatalf("provider manifest write = %d: %s", manifestPut.Code, manifestPut.Body.String())
	}
	if len(provider.readReqs) != 1 || provider.readReqs[0].MaxBytes != 64 || provider.readReqs[0].Path.String() != path.String() {
		t.Fatalf("provider read request = %#v", provider.readReqs)
	}
	if len(provider.listReqs) != 1 || provider.listReqs[0].Path.String() != appSubdir {
		t.Fatalf("provider list request = %#v", provider.listReqs)
	}
	manifestPath, _ := runtimebackend.ParseLogicalPath("workspace/app/sandbox.yaml")
	if len(provider.writeReqs) != 2 || provider.writeReqs[0].MaxBytes != 64 ||
		provider.writeReqs[0].Path.String() != path.String() ||
		provider.writeReqs[1].Path.String() != manifestPath.String() {
		t.Fatalf("provider write request = %#v", provider.writeReqs)
	}
	if got := s.providerFileReadLimit(); got != 64 {
		t.Fatalf("provider read limit = %d, want 64", got)
	}
	s.ProviderFileByteLimit = int64(maxFileBytes + 1)
	if got := s.providerFileReadLimit(); got != maxFileBytes {
		t.Fatalf("provider read limit = %d, want public cap %d", got, maxFileBytes)
	}
}

func TestProviderManifestAndInspectionUseBoundedProviderReads(t *testing.T) {
	s, provider := newProviderAPIServer(t)
	app := &store.App{ID: newULID(), OwnerToken: "tenant-a", Name: "provider app"}
	if err := s.Store.CreateApp(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	sb := createProviderSandbox(t, s, "running", app.ID)
	manifestPath, _ := runtimebackend.ParseLogicalPath("workspace/app/sandbox.yaml")
	packagePath, _ := runtimebackend.ParseLogicalPath("workspace/app/package.json")
	provider.files[manifestPath.String()] = []byte("version: 1\nweb:\n  port: 3000\n")
	provider.files[packagePath.String()] = []byte(`{"scripts":{"dev":"vite"}}`)

	manifestResponse := getManifest(s, app.ID, "tenant-a")
	if manifestResponse.Code != http.StatusOK {
		t.Fatalf("provider manifest = %d: %s", manifestResponse.Code, manifestResponse.Body.String())
	}
	inspectResponse := httptest.NewRecorder()
	inspectRequest := reqAs(http.MethodGet, "/v1/apps/"+app.ID+"/runtime-inspect", "", "tenant-a")
	inspectRequest.SetPathValue("id", app.ID)
	s.v1RuntimeInspect(inspectResponse, inspectRequest)
	if inspectResponse.Code != http.StatusOK {
		t.Fatalf("provider inspect = %d: %s", inspectResponse.Code, inspectResponse.Body.String())
	}
	if len(provider.readReqs) < 2 {
		t.Fatalf("provider manifest and inspection did not read through provider: %#v", provider.readReqs)
	}
	for _, request := range provider.readReqs {
		if request.MaxBytes != 64 {
			t.Fatalf("provider read %q used limit %d, want 64", request.Path.String(), request.MaxBytes)
		}
	}
	_ = sb
}

func TestProviderTaskBindsPrivateRuntimeWithoutDocker(t *testing.T) {
	s, provider := newProviderAPIServer(t)
	sb := createProviderSandbox(t, s, "running", "")
	response := httptest.NewRecorder()
	s.v1SubmitTask(response, providerRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/tasks", sb.ID,
		`{"prompt":"make a change","agent":"opencode"}`))
	if response.Code != http.StatusAccepted {
		t.Fatalf("provider task = %d: %s", response.Code, response.Body.String())
	}
	provider.mu.Lock()
	refs := append([]runtimebackend.SandboxRef(nil), provider.taskRefs...)
	requests := append([]runtimed.StartTaskRequest(nil), provider.taskReqs...)
	provider.mu.Unlock()
	if len(refs) == 0 || refs[0].ID != sb.ID || refs[0].RuntimeID == "" {
		t.Fatalf("provider task was not bound to its provider runtime: %#v", refs)
	}
	if len(requests) != 1 || requests[0].Prompt != "make a change" {
		t.Fatalf("provider task was not submitted through bound runtime: %#v", requests)
	}
	select {
	case <-provider.taskWatchDone:
	case <-time.After(time.Second):
		t.Fatal("provider task watcher did not use the provider task transport")
	}
}

func TestProviderTaskAdmissionPrecedesStartAndFencesStop(t *testing.T) {
	s, provider := newProviderAPIServer(t)
	sb := createProviderSandbox(t, s, "running", "")
	started := make(chan struct{})
	releaseStart := make(chan struct{})
	taskAtStart := make(chan *store.Task, 1)
	provider.startTaskHook = func(request runtimed.StartTaskRequest) {
		task, err := s.Store.GetTask(context.Background(), request.TaskID)
		if err != nil {
			t.Errorf("durable task missing at provider start: %v", err)
		} else {
			taskAtStart <- task
		}
		close(started)
		<-releaseStart
	}
	defer func() {
		select {
		case <-releaseStart:
		default:
			close(releaseStart)
		}
	}()

	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.v1SubmitTask(response, providerRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/tasks", sb.ID,
			`{"prompt":"make a change","agent":"opencode"}`))
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider StartTask was not called")
	}
	select {
	case task := <-taskAtStart:
		if task.Status != "running" || task.SandboxID != sb.ID {
			t.Fatalf("task at provider start = %#v", task)
		}
	default:
		t.Fatal("provider StartTask ran before task admission")
	}

	stop := httptest.NewRecorder()
	s.v1StopSandbox(stop, providerRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/stop", sb.ID, ""))
	if stop.Code != http.StatusConflict {
		t.Fatalf("stop during task admission = %d: %s", stop.Code, stop.Body.String())
	}
	provider.mu.Lock()
	stops := len(provider.stopRefs)
	provider.mu.Unlock()
	if stops != 0 {
		t.Fatalf("provider stop ran despite task admission lease: %d calls", stops)
	}

	close(releaseStart)
	<-done
	if response.Code != http.StatusAccepted {
		t.Fatalf("provider task = %d: %s", response.Code, response.Body.String())
	}
	select {
	case <-provider.taskWatchDone:
	case <-time.After(time.Second):
		t.Fatal("provider task watcher did not complete")
	}
}

func TestProviderTaskStartFailureFinalizesDurableAdmission(t *testing.T) {
	s, provider := newProviderAPIServer(t)
	sb := createProviderSandbox(t, s, "running", "")
	provider.startTaskErr = errors.New("private runtimed unavailable")

	response := httptest.NewRecorder()
	s.v1SubmitTask(response, providerRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/tasks", sb.ID,
		`{"prompt":"make a change","agent":"opencode"}`))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("provider start failure = %d: %s", response.Code, response.Body.String())
	}
	tasks, err := s.Store.ListTasksForSandbox(context.Background(), sb.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Status != "failed" || !tasks[0].ResultJSON.Valid ||
		!strings.Contains(tasks[0].ResultJSON.String, "task could not be started") {
		t.Fatalf("failed provider task was not finalized: %#v", tasks)
	}
	active, err := s.Store.SandboxHasRunningTask(context.Background(), sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("failed provider admission remained active")
	}
}

func TestProviderAppSandboxDefaultsProviderWebPort(t *testing.T) {
	s, provider := newProviderAPIServer(t)
	s.ProviderWebPort = 4173
	app := &store.App{ID: newULID(), OwnerToken: "tenant-a", Name: "provider app"}
	if err := s.Store.CreateApp(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	request := reqAs(http.MethodPost, "/v1/apps/"+app.ID+"/sandbox", "", "tenant-a")
	request.SetPathValue("id", app.ID)
	response := httptest.NewRecorder()
	s.v1CreateAppSandbox(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("provider app sandbox = %d: %s", response.Code, response.Body.String())
	}
	created, err := s.Store.CurrentSandboxForApp(context.Background(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Ports) != 1 || created.Ports[0] != 4173 ||
		!created.WebPort.Valid || created.WebPort.Int64 != 4173 {
		t.Fatalf("provider app sandbox port = %#v / %#v", created.Ports, created.WebPort)
	}
	provider.mu.Lock()
	creates := len(provider.createSpecs)
	provider.mu.Unlock()
	if creates != 1 {
		t.Fatalf("provider create calls = %d, want 1", creates)
	}
}

func TestProviderHostBackedOperationsAreExplicitlyUnsupported(t *testing.T) {
	s, _ := newProviderAPIServer(t)
	id := newULID()
	export := httptest.NewRecorder()
	s.v1Export(export, providerRequest(http.MethodGet, "/v1/sandboxes/"+id+"/export", id, ""))
	if export.Code != http.StatusNotImplemented {
		t.Fatalf("provider export = %d: %s", export.Code, export.Body.String())
	}
	snapshot := httptest.NewRecorder()
	s.v1CreateSnapshot(snapshot, httptest.NewRequest(http.MethodPost, "/v1/snapshots",
		strings.NewReader(`{"source_sandbox_id":"`+id+`","name":"snapshot"}`)))
	if snapshot.Code != http.StatusNotImplemented {
		t.Fatalf("provider snapshot = %d: %s", snapshot.Code, snapshot.Body.String())
	}
}

var (
	_ runtimebackend.Lifecycle              = (*providerRuntimeFake)(nil)
	_ runtimebackend.PurgeLifecycle         = (*providerRuntimeFake)(nil)
	_ runtimebackend.WorkspaceFiles         = (*providerRuntimeFake)(nil)
	_ runtimebackend.NonInteractiveExecutor = (*providerRuntimeFake)(nil)
	_ runtimebackend.TaskRuntimeBinder      = (*providerRuntimeFake)(nil)
	_ runtimebackend.TaskRuntime            = (*providerRuntimeFake)(nil)
	_ runtimebackend.PreviewReadiness       = (*providerRuntimeFake)(nil)
)
