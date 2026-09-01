package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/activity"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/agentauth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/api"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/authproxy"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/events"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/idlock"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/instancecfg"
	runtimekubernetes "github.com/tastyeffectco/sandboxd/control-plane/internal/kubernetes"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/logging"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/metrics"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/secrets"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// runKubernetes starts the production provider path. Keep this isolated from
// the Docker startup below: loading this profile must not create host
// workspaces, initialize a Docker client, or inspect the Docker socket.
func runKubernetes(log *slog.Logger) int {
	return runKubernetesProfile(log, false)
}

// runKubernetesLocal uses the same in-cluster runtime contracts as production,
// but only after the explicitly constrained Kind/SQLite/account profile has
// been validated.
func runKubernetesLocal(log *slog.Logger) int {
	return runKubernetesProfile(log, true)
}

func runKubernetesProfile(log *slog.Logger, local bool) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		config kubernetesStartupConfig
		err    error
	)
	if local {
		config, err = configuredLocalKubernetesStartup(os.Getenv)
	} else {
		config, err = configuredKubernetesStartup(os.Getenv)
	}
	if err != nil {
		log.Error("startup: Kubernetes runtime configuration invalid", "err", err.Error())
		return 1
	}

	var secretsCipher *secrets.Cipher
	if config.Local {
		if err := os.MkdirAll(config.StateDir, 0o750); err != nil {
			log.Error("startup: local Kubernetes state directory unavailable", "err", err.Error())
			return 1
		}
		secretsCipher, err = secrets.Load(strings.TrimSpace(os.Getenv("SANDBOXD_SECRETS_KEY")), config.SecretsKeyFile)
	} else {
		// Validate the key before opening PostgreSQL. Production never creates a
		// fallback keyfile, and an invalid secret configuration must fail without
		// touching durable lifecycle state.
		secretsCipher, err = secrets.LoadProduction(strings.TrimSpace(os.Getenv("SANDBOXD_SECRETS_KEY")), "")
	}
	if err != nil {
		log.Error("startup: Kubernetes secrets configuration invalid")
		return 1
	}

	// Construct the in-cluster-only adapter before opening PostgreSQL. This
	// fails closed on missing Kubernetes credentials/configuration and proves
	// the production bootstrap has no reason to initialize local runtime
	// facilities while acquiring durable state.
	runtimeAdapter, err := newInClusterRuntime(config.Runtime)
	if err != nil {
		log.Error("startup: Kubernetes runtime initialization failed", "err", err.Error())
		return 1
	}

	st, err := store.OpenWithConfig(ctx, config.Store)
	if err != nil {
		log.Error("startup: store open failed")
		return 1
	}
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			log.Error("shutdown: store close failed", "err", closeErr.Error())
		}
	}()

	auditLog := audit.New(st, log.With("component", "audit"))
	eventRec := events.New(st, log.With("component", "events"))
	authMw := auth.NewMiddleware(
		config.Auth, api.NewStoreResolver(st), auditLog, log.With("component", "auth"),
		auth.WithLoginTransactionStore(api.NewEntraLoginTransactionStore(st)),
	)
	if authMw.StartupError() != nil || (!config.Local && !authMw.ProductionReady()) ||
		(config.Local && !config.Auth.LocalAccountsReady()) {
		log.Error("startup: Kubernetes authentication configuration invalid")
		return 1
	}

	idleThreshold := config.IdleThreshold
	idleInterval := config.IdleReapInterval
	keepaliveMax := config.KeepaliveMax
	defaultAgent := envDefault("SANDBOXD_DEFAULT_AGENT", "opencode")
	live := instancecfg.New(instancecfg.Snapshot{
		IdleEnabled:          idleInterval > 0,
		IdleThresholdSeconds: int(idleThreshold.Seconds()),
		KeepaliveMaxSeconds:  int(keepaliveMax.Seconds()),
		AgentProvider:        defaultAgent,
	})
	if persisted, settingsErr := st.GetInstanceSettings(ctx); settingsErr == nil {
		agentProvider := persisted.AgentProvider
		if agentProvider == "" {
			agentProvider = defaultAgent
		}
		live.Set(instancecfg.Snapshot{
			IdleEnabled:          persisted.IdleReapEnabled,
			IdleThresholdSeconds: persisted.IdleThresholdSeconds,
			KeepaliveMaxSeconds:  persisted.KeepaliveMaxSeconds,
			AgentProvider:        agentProvider,
			DefaultModels:        persisted.AgentDefaultModels,
		})
	}

	version, gitCommit := buildIdent()
	metrics.BuildInfo.WithLabelValues(version, gitCommit).Set(1)
	if err := metrics.RefreshSandboxGauge(ctx, st); err != nil {
		log.Warn("startup: refresh sandbox gauge failed", "err", err.Error())
	}
	// The production proxy deliberately has no host credential directory or
	// writable fallback. It supports OpenCode's keyless Zen route while
	// credentialed providers are explicitly operator-managed.
	agentAuth := agentauth.NewEmptyStore()
	agentProxy := authproxy.New(agentAuth, log.With("component", "authproxy"))
	server, err := newKubernetesAPIServer(kubernetesServerConfig{
		Store:            st,
		StoreConfig:      config.Store,
		Log:              log,
		Runtime:          runtimeAdapter,
		RuntimeConfig:    config.Runtime,
		Secrets:          secretsCipher,
		AgentAuth:        agentAuth,
		Auth:             authMw,
		Audit:            auditLog,
		Events:           eventRec,
		Domain:           config.PreviewDomain,
		PublicHTTPPort:   config.PublicHTTPPort,
		LeaseHolder:      config.LeaseHolder,
		KeepaliveMax:     keepaliveMax,
		DefaultAgent:     defaultAgent,
		IdleThreshold:    idleThreshold,
		IdleReapInterval: idleInterval,
		Version:          version,
		GitCommit:        gitCommit,
		Live:             live,
		PreviewURLScheme: config.PreviewURLScheme,
		Local:            config.Local,
	})
	if err != nil {
		log.Error("startup: Kubernetes API construction invalid", "err", err.Error())
		return 1
	}

	// Constructed and fully validated before any listener is opened. In
	// particular, a malformed provider configuration cannot leave the private
	// agent-proxy port bound by a partially initialized control plane.
	agentProxyListener, err := net.Listen("tcp", "0.0.0.0:9100")
	if err != nil {
		log.Error("startup: private agent proxy listen failed", "err", err.Error())
		return 1
	}
	agentProxyServer := &http.Server{
		Handler:           agentProxy,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	agentProxyErr := make(chan error, 1)
	go func() {
		if serveErr := agentProxyServer.Serve(agentProxyListener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			agentProxyErr <- serveErr
		}
	}()

	// This is the provider equivalent of Docker reconciliation. It uses the
	// durable operation lease around each K8s inspection and has no host-side
	// effects. Task recovery reattaches through the private runtimed tunnel.
	server.ReconcileProvider(ctx)
	server.ReconcileTasks(ctx)
	server.RecoverConversations(ctx)
	_ = metrics.RefreshSandboxGauge(ctx, st)

	apiMux := authMw.Wrap(server.Handler())
	root := hostDispatch(nil, server.ProductionPreviewHandler(), server.IsPreviewHost, apiMux, log)
	root = logging.Middleware(log, root)
	httpSrv := &http.Server{
		Addr:              config.ListenAddr,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	gctx, gcancel := context.WithCancel(context.Background())
	defer gcancel()
	if config.IdleReapInterval > 0 {
		go runProviderIdleReaper(gctx, server, live, config.IdleReapInterval, log)
	}
	go runProviderReconciler(gctx, server, log)

	errCh := make(chan error, 1)
	go func() {
		log.Info("startup: Kubernetes provider listening",
			"addr", config.ListenAddr,
			"preview_domain", config.PreviewDomain,
			"runtime_class", config.Runtime.RuntimeClass,
			"local_profile", config.Local,
			"idle_threshold", idleThreshold.String(),
			"idle_interval", config.IdleReapInterval.String())
		if serveErr := httpSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				// The production path deliberately does not read an env file:
				// projected values should be rolled out as a new Pod instead.
				log.Info("reload: SIGHUP ignored in Kubernetes profile; restart Pods to rotate configuration")
				continue
			}
			log.Info("shutdown: signal received", "signal", sig.String())
		case serveErr := <-errCh:
			log.Error("shutdown: server error", "err", serveErr.Error())
		case proxyErr := <-agentProxyErr:
			log.Error("shutdown: private agent proxy error", "err", proxyErr.Error())
		}
		break
	}

	gcancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown: http server shutdown failed", "err", err.Error())
	}
	if err := agentProxyServer.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown: private agent proxy shutdown failed", "err", err.Error())
	}
	return 0
}

// kubernetesStartupConfig contains only values usable by the provider path.
// It intentionally has no data-root, Docker, loopback, or host filesystem
// fields, which makes accidental local-provider construction impossible.
type kubernetesStartupConfig struct {
	ListenAddr       string
	PreviewDomain    string
	PublicHTTPPort   string
	PreviewURLScheme string
	LeaseHolder      string
	Store            store.Config
	Runtime          runtimekubernetes.Config
	Auth             *auth.Config
	Local            bool
	StateDir         string
	SecretsKeyFile   string
	IdleThreshold    time.Duration
	IdleReapInterval time.Duration
	KeepaliveMax     time.Duration
}

func configuredKubernetesStartup(getenv environmentLookup) (kubernetesStartupConfig, error) {
	platform, err := configuredPlatform(getenv)
	if err != nil {
		return kubernetesStartupConfig{}, err
	}
	if platform != platformKubernetes {
		return kubernetesStartupConfig{}, errors.New("Kubernetes startup requires SANDBOXD_PLATFORM=kubernetes")
	}
	migrations := getenvOr(getenv, "SANDBOXD_MIGRATIONS", migrationsDir)
	storeConfig, err := configuredStoreConfig(getenv, "", migrations)
	if err != nil {
		return kubernetesStartupConfig{}, err
	}
	if storeConfig.Profile != store.ProfileProduction {
		return kubernetesStartupConfig{}, errors.New("Kubernetes startup requires PostgreSQL persistence")
	}
	authConfig := auth.ParseConfig(getenv)
	if authConfig.Profile != auth.ProfileEntra || !authConfig.ProductionReady() {
		return kubernetesStartupConfig{}, errors.New("Kubernetes startup requires complete Entra authentication")
	}
	runtimeConfig, err := configuredKubernetesRuntime(getenv)
	if err != nil {
		return kubernetesStartupConfig{}, err
	}
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(getenv("PREVIEW_DOMAIN")), "."))
	if domain == "" || domain == "localhost" {
		return kubernetesStartupConfig{}, errors.New("Kubernetes startup requires a non-local PREVIEW_DOMAIN")
	}
	if strings.TrimSpace(getenv("PREVIEW_URL_SCHEME")) != "https" {
		return kubernetesStartupConfig{}, errors.New("Kubernetes startup requires PREVIEW_URL_SCHEME=https")
	}
	port := getenvOr(getenv, "SANDBOXD_PUBLIC_HTTP_PORT", "443")
	if err := validPublicPort(port); err != nil {
		return kubernetesStartupConfig{}, err
	}
	idleThreshold, err := durationFromEnvSecStrict(getenv, "SANDBOXD_IDLE_THRESHOLD_SECONDS", defaultIdleThresholdSec)
	if err != nil {
		return kubernetesStartupConfig{}, err
	}
	idleReapInterval, err := durationFromEnvSecStrict(getenv, "SANDBOXD_IDLE_REAP_INTERVAL_SECONDS", defaultIdleReapIntervalSec)
	if err != nil {
		return kubernetesStartupConfig{}, err
	}
	keepaliveMax, err := durationFromEnvSecStrict(getenv, "SANDBOXD_KEEPALIVE_MAX_SECONDS", defaultKeepaliveMaxSec)
	if err != nil {
		return kubernetesStartupConfig{}, err
	}
	holder := strings.TrimSpace(getenv("SANDBOXD_RUNTIME_LEASE_HOLDER"))
	if holder == "" || holder == "sandboxd-single-process" {
		return kubernetesStartupConfig{}, errors.New("Kubernetes startup requires a unique SANDBOXD_RUNTIME_LEASE_HOLDER")
	}
	return kubernetesStartupConfig{
		ListenAddr:       getenvOr(getenv, "SANDBOXD_ADDR", defaultListenAddr),
		PreviewDomain:    domain,
		PublicHTTPPort:   port,
		PreviewURLScheme: "https",
		LeaseHolder:      holder,
		Store:            storeConfig,
		Runtime:          runtimeConfig,
		Auth:             authConfig,
		IdleThreshold:    idleThreshold,
		IdleReapInterval: idleReapInterval,
		KeepaliveMax:     keepaliveMax,
	}, nil
}

// configuredLocalKubernetesStartup accepts only the Kind development profile.
// It intentionally has its own validation path so production's PostgreSQL,
// Entra, HTTPS, and Kata requirements remain unchanged.
func configuredLocalKubernetesStartup(getenv environmentLookup) (kubernetesStartupConfig, error) {
	platform, err := configuredPlatform(getenv)
	if err != nil {
		return kubernetesStartupConfig{}, err
	}
	if platform != platformKubernetesLocal {
		return kubernetesStartupConfig{}, errors.New("local Kubernetes startup requires SANDBOXD_PLATFORM=kubernetes-local")
	}
	if strings.TrimSpace(getenv("DATABASE_URL")) != "" {
		return kubernetesStartupConfig{}, errors.New("local Kubernetes startup requires SQLite, not DATABASE_URL")
	}
	dbPath := strings.TrimSpace(getenv("SANDBOXD_DB"))
	if !filepath.IsAbs(dbPath) {
		return kubernetesStartupConfig{}, errors.New("local Kubernetes startup requires an absolute SANDBOXD_DB path")
	}
	stateDir := filepath.Dir(filepath.Clean(dbPath))
	keyFile := strings.TrimSpace(getenv("SANDBOXD_SECRETS_KEY_FILE"))
	if !filepath.IsAbs(keyFile) || !pathWithin(stateDir, keyFile) {
		return kubernetesStartupConfig{}, errors.New("local Kubernetes startup requires SANDBOXD_SECRETS_KEY_FILE within the SQLite state directory")
	}
	migrations := getenvOr(getenv, "SANDBOXD_MIGRATIONS", migrationsDir)
	storeConfig, err := configuredStoreConfig(getenv, dbPath, migrations)
	if err != nil {
		return kubernetesStartupConfig{}, err
	}
	if storeConfig.Provider != store.ProviderSQLite || storeConfig.Profile == store.ProfileProduction {
		return kubernetesStartupConfig{}, errors.New("local Kubernetes startup requires SQLite persistence")
	}
	authConfig := auth.ParseConfig(getenv)
	if !authConfig.LocalAccountsReady() {
		return kubernetesStartupConfig{}, errors.New("local Kubernetes startup requires SANDBOXD_LOCAL_AUTH_MODE=accounts with authentication enabled and no API tokens")
	}
	if strings.TrimSpace(getenv("SANDBOXD_CONTROL_PLANE_REPLICAS")) != "1" {
		return kubernetesStartupConfig{}, errors.New("local Kubernetes startup requires SANDBOXD_CONTROL_PLANE_REPLICAS=1 for SQLite")
	}
	runtimeConfig, err := configuredLocalKubernetesRuntime(getenv)
	if err != nil {
		return kubernetesStartupConfig{}, err
	}
	if runtimeConfig.StorageClass != "standard" {
		return kubernetesStartupConfig{}, errors.New("local Kubernetes startup requires SANDBOXD_KUBERNETES_STORAGE_CLASS=standard")
	}
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(getenv("PREVIEW_DOMAIN")), "."))
	if domain != "localhost" {
		return kubernetesStartupConfig{}, errors.New("local Kubernetes startup requires PREVIEW_DOMAIN=localhost")
	}
	if strings.TrimSpace(getenv("PREVIEW_URL_SCHEME")) != "http" {
		return kubernetesStartupConfig{}, errors.New("local Kubernetes startup requires PREVIEW_URL_SCHEME=http")
	}
	port := strings.TrimSpace(getenv("SANDBOXD_PUBLIC_HTTP_PORT"))
	if port != "9090" {
		return kubernetesStartupConfig{}, errors.New("local Kubernetes startup requires SANDBOXD_PUBLIC_HTTP_PORT=9090")
	}
	if err := validPublicPort(port); err != nil {
		return kubernetesStartupConfig{}, err
	}
	idleThreshold, err := durationFromEnvSecStrict(getenv, "SANDBOXD_IDLE_THRESHOLD_SECONDS", defaultIdleThresholdSec)
	if err != nil {
		return kubernetesStartupConfig{}, err
	}
	idleReapInterval, err := durationFromEnvSecStrict(getenv, "SANDBOXD_IDLE_REAP_INTERVAL_SECONDS", defaultIdleReapIntervalSec)
	if err != nil {
		return kubernetesStartupConfig{}, err
	}
	keepaliveMax, err := durationFromEnvSecStrict(getenv, "SANDBOXD_KEEPALIVE_MAX_SECONDS", defaultKeepaliveMaxSec)
	if err != nil {
		return kubernetesStartupConfig{}, err
	}
	holder := strings.TrimSpace(getenv("SANDBOXD_RUNTIME_LEASE_HOLDER"))
	if holder == "" || holder == "sandboxd-single-process" {
		return kubernetesStartupConfig{}, errors.New("local Kubernetes startup requires a unique SANDBOXD_RUNTIME_LEASE_HOLDER")
	}
	return kubernetesStartupConfig{
		ListenAddr:       getenvOr(getenv, "SANDBOXD_ADDR", defaultListenAddr),
		PreviewDomain:    domain,
		PublicHTTPPort:   port,
		PreviewURLScheme: "http",
		LeaseHolder:      holder,
		Store:            storeConfig,
		Runtime:          runtimeConfig,
		Auth:             authConfig,
		Local:            true,
		StateDir:         stateDir,
		SecretsKeyFile:   keyFile,
		IdleThreshold:    idleThreshold,
		IdleReapInterval: idleReapInterval,
		KeepaliveMax:     keepaliveMax,
	}, nil
}

func pathWithin(parent, child string) bool {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validPublicPort(raw string) error {
	port, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 16)
	if err != nil || port == 0 {
		return errors.New("SANDBOXD_PUBLIC_HTTP_PORT must be a valid TCP port")
	}
	return nil
}

func durationFromEnvSecStrict(getenv environmentLookup, key string, fallback int) (time.Duration, error) {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return time.Duration(fallback) * time.Second, nil
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds < 0 || seconds > int64((1<<63-1)/int64(time.Second)) {
		return 0, fmt.Errorf("%s must be a non-negative number of seconds", key)
	}
	return time.Duration(seconds) * time.Second, nil
}

// runtimeFactory is a seam for startup tests. Its production value uses only
// Kubernetes in-cluster credentials and never consults Docker.
type runtimeFactory func(runtimekubernetes.Config) (*runtimekubernetes.Adapter, error)

var newInClusterRuntime runtimeFactory = runtimekubernetes.NewInCluster

type kubernetesServerConfig struct {
	Store            *store.Store
	StoreConfig      store.Config
	Log              *slog.Logger
	Runtime          *runtimekubernetes.Adapter
	RuntimeConfig    runtimekubernetes.Config
	Secrets          *secrets.Cipher
	AgentAuth        *agentauth.Store
	Auth             *auth.Middleware
	Audit            *audit.Logger
	Events           *events.Recorder
	Domain           string
	PublicHTTPPort   string
	LeaseHolder      string
	KeepaliveMax     time.Duration
	DefaultAgent     string
	IdleThreshold    time.Duration
	IdleReapInterval time.Duration
	Version          string
	GitCommit        string
	Live             *instancecfg.Live
	PreviewURLScheme string
	Local            bool
}

func newKubernetesAPIServer(config kubernetesServerConfig) (*api.Server, error) {
	if err := validateKubernetesServerConfig(config); err != nil {
		return nil, err
	}
	runtime := config.Runtime
	agentAuth := config.AgentAuth
	if agentAuth == nil {
		agentAuth = agentauth.NewEmptyStore()
	}
	return &api.Server{
		Store:                  config.Store,
		Secrets:                config.Secrets,
		Log:                    config.Log.With("component", "api"),
		PreviewDomain:          config.Domain,
		Image:                  config.RuntimeConfig.SandboxImage,
		PreviewURLScheme:       config.PreviewURLScheme,
		AllowInsecurePreview:   config.Local,
		PublicHTTPPort:         config.PublicHTTPPort,
		Inflight:               activity.NewInflightExec(),
		Locks:                  idlock.New(),
		KeepaliveMax:           config.KeepaliveMax,
		Auth:                   config.Auth,
		Audit:                  config.Audit,
		Events:                 config.Events,
		DefaultAgent:           config.DefaultAgent,
		AgentAuth:              agentAuth,
		AgentProxyURL:          config.RuntimeConfig.AgentProxyURL,
		RuntimeLifecycle:       runtime,
		RuntimePurger:          runtime,
		RuntimeFiles:           runtime,
		RuntimeProcessLogs:     runtime,
		RuntimeExec:            runtime,
		RuntimeTTY:             runtime,
		TaskRuntime:            runtime,
		PreviewRuntime:         runtime,
		PreviewReadiness:       runtime,
		RuntimeLeaseTTL:        45 * time.Second,
		RuntimeLeaseHolder:     config.LeaseHolder,
		ProviderWebPort:        int(config.RuntimeConfig.WebPort),
		ProviderFileByteLimit:  config.RuntimeConfig.MaxFileBytes,
		ProviderFileEntryLimit: config.RuntimeConfig.MaxFileEntries,
		PreviewClusterDomain:   config.RuntimeConfig.ClusterDomain,
		Instance: api.InstanceInfo{
			Version:              config.Version,
			GitCommit:            config.GitCommit,
			AuthEnabled:          true,
			StorageMode:          "kubernetes-pvc",
			EgressMode:           "disabled",
			AgentProviders:       []string{"opencode", "claude-code", "codex"},
			IdleReapEnabled:      config.IdleReapInterval > 0,
			IdleThresholdSeconds: int(config.IdleThreshold.Seconds()),
		},
		Live: config.Live,
	}, nil
}

func validateKubernetesServerConfig(config kubernetesServerConfig) error {
	if config.Store == nil {
		return errors.New("Kubernetes API server requires a durable store")
	}
	if config.Local {
		if config.StoreConfig.Provider != store.ProviderSQLite || config.StoreConfig.Profile == store.ProfileProduction {
			return errors.New("local Kubernetes API server requires SQLite store configuration")
		}
	} else if config.StoreConfig.Profile != store.ProfileProduction {
		return errors.New("Kubernetes API server requires PostgreSQL store configuration")
	}
	if config.Log == nil {
		return errors.New("Kubernetes API server requires a logger")
	}
	if config.Runtime == nil {
		return errors.New("Kubernetes API server requires a runtime adapter")
	}
	if err := config.RuntimeConfig.Validate(); err != nil {
		return fmt.Errorf("Kubernetes API server runtime configuration: %w", err)
	}
	if strings.TrimSpace(config.RuntimeConfig.AgentProxyURL) == "" {
		return errors.New("Kubernetes API server requires a private runtime agent proxy")
	}
	if config.Secrets == nil {
		return errors.New("Kubernetes API server requires encryption secrets")
	}
	if config.Auth == nil || config.Auth.StartupError() != nil {
		return errors.New("Kubernetes API server requires configured authentication")
	}
	if !config.Local && !config.Auth.ProductionReady() {
		return errors.New("Kubernetes API server requires complete Entra authentication")
	}
	if config.Local && !config.Auth.LocalAccountsReady() {
		return errors.New("local Kubernetes API server requires local account authentication")
	}
	if config.Audit == nil || config.Events == nil {
		return errors.New("Kubernetes API server requires durable audit and event recorders")
	}
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(config.Domain), "."))
	if !config.Local && (domain == "" || domain == "localhost") {
		return errors.New("Kubernetes API server requires a non-local preview domain")
	}
	if config.Local && domain != "localhost" {
		return errors.New("local Kubernetes API server requires PREVIEW_DOMAIN=localhost")
	}
	if err := validPublicPort(config.PublicHTTPPort); err != nil {
		return err
	}
	if strings.TrimSpace(config.LeaseHolder) == "" || config.LeaseHolder == "sandboxd-single-process" {
		return errors.New("Kubernetes API server requires a unique runtime lease holder")
	}
	if !config.Local && config.PreviewURLScheme != "https" {
		return errors.New("Kubernetes API server requires HTTPS previews")
	}
	if config.Local && (config.PreviewURLScheme != "http" || config.PublicHTTPPort != "9090") {
		return errors.New("local Kubernetes API server requires HTTP previews on port 9090")
	}
	return nil
}

func runProviderIdleReaper(ctx context.Context, server *api.Server, live *instancecfg.Live, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if live.IdleEnabled() {
				server.ReapProviderIdle(ctx, live.IdleThreshold())
			}
		}
	}
}

func runProviderReconciler(ctx context.Context, server *api.Server, log *slog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			server.ReconcileProvider(ctx)
			server.ReconcileTasks(ctx)
			if err := metrics.RefreshSandboxGauge(ctx, server.Store); err != nil {
				log.Warn("provider reconcile: refresh sandbox gauge failed", "err", err.Error())
			}
		}
	}
}
