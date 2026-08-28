package copilot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type catalogRuntime struct {
	fakeRuntime

	mu            sync.Mutex
	models        []ModelInfo
	err           error
	calls         int
	invalidations int
	started       chan<- struct{}
	release       <-chan struct{}
}

func (r *catalogRuntime) ListModels(ctx context.Context, _ string) ([]ModelInfo, error) {
	r.mu.Lock()
	r.calls++
	models, err := cloneModelCatalog(r.models), r.err
	started, release := r.started, r.release
	r.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return models, err
}

func (r *catalogRuntime) InvalidateModelCatalog() {
	r.mu.Lock()
	r.invalidations++
	r.mu.Unlock()
}

func (r *catalogRuntime) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *catalogRuntime) invalidationCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.invalidations
}

func connectedCatalogManager(t *testing.T, runtime *catalogRuntime) *Manager {
	t.Helper()
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	manager := newTestManager(t, &now, runtime, nil)
	manager.mu.Lock()
	manager.credential = &credential{PersonalAccessToken: "catalog-token", Account: "octocat"}
	manager.mu.Unlock()
	return manager
}

func TestListModelsSanitizesAndCachesCatalog(t *testing.T) {
	limit := 128000
	runtime := &catalogRuntime{models: []ModelInfo{
		{
			ID: "gpt-5.3-codex", Name: " GPT-5.3 Codex ",
			SupportedReasoningEfforts: []string{"low", "high", "low", " "},
			DefaultReasoningEffort:    "high",
			MaxContextWindowTokens:    &limit,
		},
		{ID: " invalid", Name: "not valid"},
	}}
	manager := connectedCatalogManager(t, runtime)

	first, err := manager.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Name != "GPT-5.3 Codex" ||
		first[0].DefaultReasoningEffort != "high" ||
		len(first[0].SupportedReasoningEfforts) != 2 ||
		*first[0].MaxContextWindowTokens != limit {
		t.Fatalf("sanitized catalog = %#v", first)
	}
	first[0].SupportedReasoningEfforts[0] = "mutated"
	*first[0].MaxContextWindowTokens = 1

	second, err := manager.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.callCount() != 1 {
		t.Fatalf("catalog calls = %d; want one cached lookup", runtime.callCount())
	}
	if second[0].SupportedReasoningEfforts[0] != "low" || *second[0].MaxContextWindowTokens != limit {
		t.Fatalf("catalog cache leaked mutable state: %#v", second)
	}
}

func TestValidateModelSelectionRejectsIncompatibleControls(t *testing.T) {
	runtime := &catalogRuntime{models: []ModelInfo{{
		ID: "gpt-5.3-codex", Name: "GPT-5.3 Codex",
		SupportedReasoningEfforts: []string{"low", "high"},
		DefaultReasoningEffort:    "high",
	}}}
	manager := connectedCatalogManager(t, runtime)

	selection, err := manager.ValidateModelSelection(context.Background(),
		" gpt-5.3-codex ", " high ", ContextTierLongContext)
	if err != nil {
		t.Fatal(err)
	}
	if selection != (ModelSelection{
		Model: "gpt-5.3-codex", ReasoningEffort: "high", ContextTier: ContextTierLongContext,
	}) {
		t.Fatalf("selection = %#v", selection)
	}
	for _, input := range []struct {
		model, effort, tier string
	}{
		{"missing", "", ContextTierDefault},
		{"gpt-5.3-codex", "medium", ContextTierDefault},
		{"", "high", ContextTierDefault},
		{"", "", ContextTierLongContext},
		{"gpt-5.3-codex", "", "unsupported"},
	} {
		if _, err := manager.ValidateModelSelection(context.Background(), input.model, input.effort, input.tier); !errors.Is(err, ErrInvalidModelSelection) {
			t.Errorf("ValidateModelSelection(%q, %q, %q) = %v; want ErrInvalidModelSelection",
				input.model, input.effort, input.tier, err)
		}
	}
	if runtime.callCount() != 1 {
		t.Fatalf("catalog calls = %d; expected validation to use its cache", runtime.callCount())
	}
}

func TestListModelsInvalidatesAfterDisconnect(t *testing.T) {
	runtime := &catalogRuntime{models: []ModelInfo{{ID: "gpt-5.3-codex", Name: "GPT-5.3 Codex"}}}
	manager := connectedCatalogManager(t, runtime)
	if _, err := manager.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ListModels(context.Background()); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("catalog lookup after disconnect = %v; want ErrNotConnected", err)
	}
	if runtime.invalidationCount() != 1 {
		t.Fatalf("runtime catalog invalidations = %d; want 1", runtime.invalidationCount())
	}
}

func TestListModelsSurfacesProviderFailure(t *testing.T) {
	runtime := &catalogRuntime{err: errors.New("provider unavailable")}
	manager := connectedCatalogManager(t, runtime)
	if _, err := manager.ListModels(context.Background()); !errors.Is(err, ErrModelCatalogUnavailable) {
		t.Fatalf("catalog lookup error = %v; want ErrModelCatalogUnavailable", err)
	}
}

func TestListModelsCoalescesConcurrentLookups(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	runtime := &catalogRuntime{
		models:  []ModelInfo{{ID: "gpt-5.3-codex", Name: "GPT-5.3 Codex"}},
		started: started,
		release: release,
	}
	manager := connectedCatalogManager(t, runtime)
	firstDone := make(chan error, 1)
	go func() {
		_, err := manager.ListModels(context.Background())
		firstDone <- err
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := manager.ListModels(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent lookup error = %v; want context deadline", err)
	}
	if runtime.callCount() != 1 {
		t.Fatalf("runtime catalog calls = %d; concurrent requests were not coalesced", runtime.callCount())
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first lookup = %v", err)
	}
}

func TestConnectPATInvalidatesCachedModelCatalog(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"login":"hubot"}`))
	}))
	defer server.Close()
	runtime := &catalogRuntime{models: []ModelInfo{{ID: "gpt-5.3-codex", Name: "GPT-5.3 Codex"}}}
	manager := newTestManager(t, &now, runtime, server.Client(), server.URL)
	manager.mu.Lock()
	manager.credential = &credential{PersonalAccessToken: "old-token", Account: "octocat"}
	manager.mu.Unlock()
	if _, err := manager.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ConnectPAT(context.Background(), testFineGrainedPAT); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.callCount() != 2 {
		t.Fatalf("catalog calls = %d; expected token replacement to invalidate cache", runtime.callCount())
	}
	if runtime.invalidationCount() != 1 {
		t.Fatalf("runtime catalog invalidations = %d; want 1", runtime.invalidationCount())
	}
}
