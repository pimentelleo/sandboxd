package main

import (
	"context"
	"encoding/base64"
	"log/slog"
	"testing"
	"time"

	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/api"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/events"
	runtimekubernetes "github.com/tastyeffectco/sandboxd/control-plane/internal/kubernetes"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/secrets"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

func TestNewKubernetesAPIServerUsesOnlyProviderCollaborators(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, "file::memory:?_fk=1", "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	env := productionEnvironment()
	runtimeConfig, err := configuredKubernetesRuntime(envLookup(env))
	if err != nil {
		t.Fatalf("configuredKubernetesRuntime: %v", err)
	}
	adapter, err := runtimekubernetes.NewAdapter(runtimeConfig, k8sfake.NewSimpleClientset())
	if err != nil {
		t.Fatalf("new Kubernetes adapter: %v", err)
	}
	authConfig := auth.ParseConfig(envLookup(env))
	auditLog := audit.New(st, slog.Default())
	authMiddleware := auth.NewMiddleware(
		authConfig, api.NewStoreResolver(st), auditLog, slog.Default(),
		auth.WithLoginTransactionStore(api.NewEntraLoginTransactionStore(st)),
	)
	if !authMiddleware.ProductionReady() {
		t.Fatal("production auth middleware is not ready")
	}
	cipher, err := secrets.LoadProduction(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatalf("load production cipher: %v", err)
	}

	server, err := newKubernetesAPIServer(kubernetesServerConfig{
		Store:            st,
		StoreConfig:      store.Config{Profile: store.ProfileProduction, DSN: env["DATABASE_URL"]},
		Log:              slog.Default(),
		Runtime:          adapter,
		RuntimeConfig:    runtimeConfig,
		Secrets:          cipher,
		Auth:             authMiddleware,
		Audit:            auditLog,
		Events:           events.New(st, slog.Default()),
		Domain:           env["PREVIEW_DOMAIN"],
		PublicHTTPPort:   env["SANDBOXD_PUBLIC_HTTP_PORT"],
		LeaseHolder:      env["SANDBOXD_RUNTIME_LEASE_HOLDER"],
		KeepaliveMax:     time.Hour,
		IdleThreshold:    time.Hour,
		IdleReapInterval: time.Minute,
		PreviewURLScheme: "https",
	})
	if err != nil {
		t.Fatalf("newKubernetesAPIServer: %v", err)
	}

	if server.Docker != nil || server.Loopback != nil {
		t.Fatal("Kubernetes server must not construct Docker or host workspace collaborators")
	}
	if server.RuntimeLifecycle != adapter || server.RuntimePurger != adapter ||
		server.RuntimeFiles != adapter || server.RuntimeProcessLogs != adapter ||
		server.RuntimeExec != adapter ||
		server.RuntimeTTY != adapter || server.TaskRuntime != adapter ||
		server.PreviewRuntime != adapter || server.PreviewReadiness != adapter {
		t.Fatal("Kubernetes server did not wire the provider adapter for all runtime paths")
	}
	if server.ProviderFileByteLimit != runtimeConfig.MaxFileBytes {
		t.Fatalf("provider file byte limit = %d, want %d", server.ProviderFileByteLimit, runtimeConfig.MaxFileBytes)
	}
}
