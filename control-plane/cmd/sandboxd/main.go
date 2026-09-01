// sandboxd — control plane for the sandboxd.
//
// A single Go binary. It listens on the internal network (default
// 0.0.0.0:9000, see defaultListenAddr), with auth enforced by the
// internal/auth middleware, and shells out to the `docker` CLI via
// os/exec.
//
// Phase 4 added: SQLite migrations + open, boot-time reconciler,
// HTTP API on 127.0.0.1, signal handling, graceful shutdown.
//
// Phase 5 adds: access-log tailer goroutine, optional Traefik
// open-connection poller goroutine (fallback if the metric can't be
// verified), the 30-second idle reaper, the 10-second host-memory
// pressure reaper, and the /wake/{id} handler (both Traefik
// catch-all and programmatic shapes).
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/activity"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/agentauth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/api"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/authproxy"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/copilot"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/egress"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/events"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/idlock"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/instancecfg"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/logging"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/loopback"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/metrics"
	nginxwatch "github.com/tastyeffectco/sandboxd/control-plane/internal/nginx"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/reaper"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/reconcile"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/sandboxname"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/sandboxspec"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/secrets"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/snapshot"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/telemetry"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/upgrade"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/wake"
)

const (
	// OSS default: sandboxd runs in its own container and Traefik
	// reaches it over the compose network by service name, so it binds
	// all interfaces (it is NOT published to the host — only reachable
	// on the internal sandboxd_net). Override with SANDBOXD_ADDR.
	defaultListenAddr = "0.0.0.0:9000"
	defaultImage      = "sandboxd-base:0.3.0"
	migrationsDir     = "/usr/local/share/sandboxd/migrations"

	// Default data root for the portable build. The compose file
	// bind-mounts this path host:container symmetric, so it is a valid
	// host path for the sibling `docker run -v`. Override with
	// SANDBOXD_DATA_DIR / SANDBOXD_LOG_DIR (paths derived in main()).
	defaultDataDir = "/var/lib/sandboxd"
	defaultLogDir  = "/var/log/sandboxd"

	// The project used to be called "sandboxed"; installs from before the
	// rename keep their data where it is. Never move data silently.
	legacyDataDir = "/var/lib/sandboxed"
	legacyLogDir  = "/var/log/sandboxed"
	legacyEtcDir  = "/etc/sandboxed"

	// Idle / pressure / wake defaults. Each overridable via env; see
	// .env.example and README "Configuration".
	defaultIdleThresholdSec    = 2100 // 35 min idle → docker stop
	defaultIdleReapIntervalSec = 30
	defaultPressureIntervalSec = 10
	defaultMemHeadroomPct      = 15
	defaultMemRefusePct        = 10
	defaultMemEmergencyPct     = 5
	defaultWakeCostMB          = 800
	defaultWakeTCPReadySec     = 8
	defaultWakeGraceSec        = 60
	defaultKeepaliveMaxSec     = 86400

	// Snapshot subsystem defaults (auto-snapshotter is disabled in the
	// OSS build — see main(); these remain for the manual API surface).
	defaultSnapshotIntervalSec   = 3600
	defaultSnapshotRetentionDays = 7
	defaultSnapshotIdleHours     = 24
)

func main() {
	// One-shot subcommands run and exit before the daemon startup path.
	if len(os.Args) > 1 {
		if isMigrateCommand(os.Args[1:]) {
			os.Exit(runMigrate(
				os.Args[2:],
				os.Getenv,
				openMigrationStore,
				configuredMigrationsDir(),
				os.Stdout,
				os.Stderr,
			))
		}
		switch os.Args[1] {
		case "backfill-legacy":
			os.Exit(runBackfillLegacy(os.Args[2:]))
		case "version", "--version", "-v":
			v, c := buildIdent()
			fmt.Printf("sandboxd %s (%s)\n", v, c)
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "sandboxd: unknown subcommand %q\n", os.Args[1])
			os.Exit(2)
		}
	}

	log := logging.NewLogger()
	platform, err := configuredPlatform(os.Getenv)
	if err != nil {
		log.Error("startup: platform configuration invalid", "err", err.Error())
		os.Exit(1)
	}
	if platform == platformKubernetes {
		os.Exit(runKubernetes(log))
	}
	if platform == platformKubernetesLocal {
		os.Exit(runKubernetesLocal(log))
	}
	if err := validateLocalPlatformProfile(os.Getenv); err != nil {
		log.Error("startup: local platform configuration invalid", "err", err.Error())
		os.Exit(1)
	}

	addr := envDefault("SANDBOXD_ADDR", defaultListenAddr)
	image := envDefault("SANDBOXD_IMAGE", defaultImage)
	// OSS default preview domain is "localhost": browsers resolve any
	// *.localhost name to 127.0.0.1, so preview URLs work out of the box
	// with no DNS and no certificates. Set PREVIEW_DOMAIN to a real
	// wildcard domain (plus PREVIEW_ENTRYPOINT=websecure, PREVIEW_TLS=true)
	// for a public deployment.
	domain := envDefault("PREVIEW_DOMAIN", "localhost")

	// Data + log roots. All per-sandbox workspace paths derive from
	// dataDir; the compose file bind-mounts dataDir host:container
	// symmetric so the path sandboxd writes is also a valid host path
	// for the sibling `docker run -v <workspace>:/home/sandbox`.
	dataDir := envDefault("SANDBOXD_DATA_DIR", legacyAware(defaultDataDir, legacyDataDir))
	logDir := envDefault("SANDBOXD_LOG_DIR", legacyAware(defaultLogDir, legacyLogDir))
	stateDir := filepath.Join(dataDir, "state")
	dbPath := filepath.Join(stateDir, "sandboxd.db")
	workspacesRoot := filepath.Join(dataDir, "workspaces")
	snapshotsRoot := filepath.Join(dataDir, "_snapshots")
	templatesRoot := filepath.Join(dataDir, "templates")
	libraryRoot := filepath.Join(dataDir, "library")
	accessLogPath := filepath.Join(logDir, "traefik-access.log")
	tailerOffsetFs := filepath.Join(stateDir, "traefik-tail.offset")

	// OSS docker-native toggles (default to the portable behaviour).
	network := os.Getenv("SANDBOXD_NETWORK")        // shared docker network for Traefik routing
	userns := envDefault("SANDBOXD_USERNS", "host") // sandbox + seed --userns; "host" is deterministic on any daemon
	previewEntrypoint := envDefault("PREVIEW_ENTRYPOINT", "web")
	previewTLS := boolFromEnv("PREVIEW_TLS", false)
	previewURLScheme := strings.ToLower(strings.TrimSpace(os.Getenv("PREVIEW_URL_SCHEME")))
	if previewURLScheme != "" && previewURLScheme != "http" && previewURLScheme != "https" {
		fmt.Fprintf(os.Stderr, "PREVIEW_URL_SCHEME must be http, https, or empty (got %q)\n", previewURLScheme)
		os.Exit(2)
	}
	// Host-facing port the preview/console URLs are reached on (compose passes
	// the published HTTP_PORT here). Default "80": bare URLs on a dedicated host.
	publicHTTPPort := envDefault("SANDBOXD_PUBLIC_HTTP_PORT", "80")
	setMemoryHigh := boolFromEnv("SANDBOXD_SET_MEMORY_HIGH", false)

	migrations := configuredMigrationsDir()

	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		log.Error("startup: mkdir state dir failed", "err", err.Error())
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeConfig, err := configuredStoreConfig(os.Getenv, dbPath, migrations)
	if err != nil {
		log.Error("startup: store configuration invalid", "err", err.Error())
		os.Exit(1)
	}
	st, err := store.OpenWithConfig(ctx, storeConfig)
	if err != nil {
		if storeConfig.Profile == store.ProfileProduction {
			log.Error("startup: store open failed")
		} else {
			log.Error("startup: store open failed", "err", err.Error())
		}
		os.Exit(1)
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("shutdown: store close failed", "err", err.Error())
		}
	}()

	// Phase 5 — Backfill last_active_at for legacy running rows where
	// the migration default (0) would otherwise make every existing
	// row an idle candidate the moment the daemon comes up.
	if n, err := st.BackfillRunningActivity(ctx); err != nil {
		log.Warn("startup: backfill last_active_at failed", "err", err.Error())
	} else if n > 0 {
		log.Info("startup: backfilled last_active_at for running rows", "rows", n)
	}

	// Phase 8 — audit logger + service-token auth middleware. The
	// initial config is read from the process environment (systemd has
	// already loaded the EnvironmentFile); SIGHUP re-reads the file.
	auditLog := audit.New(st, log.With("component", "audit"))
	eventRec := events.New(st, log.With("component", "events"))
	envFile := envDefault("SANDBOXD_ENV_FILE", legacyAware("/etc/sandboxd/sandboxd.env", legacyEtcDir+"/sandboxd.env"))
	authCfg := auth.ParseConfig(os.Getenv)
	// The credential resolver validates DB-backed console sessions and API keys;
	// env-configured SANDBOXD_API_TOKENS remain a fallback for the bootstrap key.
	authMw := auth.NewMiddleware(
		authCfg, api.NewStoreResolver(st), auditLog, log.With("component", "auth"),
		auth.WithLoginTransactionStore(api.NewEntraLoginTransactionStore(st)),
	)
	if authCfg.Profile == auth.ProfileEntra && storeConfig.Profile != store.ProfileProduction {
		log.Error("startup: production authentication requires PostgreSQL persistence")
		os.Exit(1)
	}
	if err := authMw.StartupError(); err != nil {
		log.Error("startup: production authentication is not configured")
		os.Exit(1)
	}
	denyMode := envDefault("SANDBOXD_FORWARD_AUTH_DENY_MODE", "redirect")
	{
		ac := authMw.Snapshot()
		log.Info("startup: auth configured",
			"api_tokens", len(ac.APITokens),
			"preview_secrets", len(ac.PreviewSecrets),
			"auth_disabled", ac.Disabled,
			"forward_auth_deny_mode", denyMode,
		)
		if len(ac.APITokens) == 0 && !ac.Disabled {
			log.Warn("startup: SANDBOXD_API_TOKENS is empty — every external API call will 401 (loopback still works)")
		}
	}

	dockerClient := docker.NewClient()
	loopMgr := loopback.New()
	loopMgr.Root = workspacesRoot
	loopMgr.SeedImage = image
	loopMgr.DockerBin = "docker"
	loopMgr.Userns = userns
	loopMgr.Log = log.With("component", "loopback")

	// Egress (nftables) is DISABLED in the portable OSS build: it
	// requires host nftables + journald + systemd-timer refresh jobs
	// that don't exist in a plain docker-compose deployment. Every
	// consumer (reconciler, reapers, wake, API) treats a nil egress
	// manager as "no egress policy" — exactly the default-allow
	// behaviour, minus the connection logging. A nil manager here is
	// the single switch that keeps all of that off.
	var egressMgr *egress.Manager = nil

	// Phase 7 — shared per-id lock + snapshot subsystem. The snapshot
	// Manager is handed to the reconciler so its boot pass can sweep
	// crash debris (stale .tmp snapshots, interrupted-restore .bak).
	idLocks := idlock.New()
	snapshotMgr := &snapshot.Manager{
		WorkspacesRoot: workspacesRoot,
		SnapshotsRoot:  snapshotsRoot,
		RetentionDays:  intFromEnv("SANDBOXD_SNAPSHOT_RETENTION_DAYS", defaultSnapshotRetentionDays),
		IdleThreshold:  time.Duration(intFromEnv("SANDBOXD_SNAPSHOT_IDLE_HOURS", defaultSnapshotIdleHours)) * time.Hour,
		Store:          st,
		Locks:          idLocks,
		Log:            log.With("component", "snapshot"),
	}

	version, gitCommit := buildIdent()
	metrics.BuildInfo.WithLabelValues(version, gitCommit).Set(1)
	if err := metrics.RefreshSandboxGauge(ctx, st); err != nil {
		log.Warn("startup: refresh sandbox gauge failed", "err", err.Error())
	}

	// Reconciler once, before the listener.
	rcDeps := reconcile.Deps{
		Store:         st,
		Docker:        dockerClient,
		Loopback:      loopMgr,
		Egress:        egressMgr,
		Snapshot:      snapshotMgr,
		Log:           log.With("component", "reconcile"),
		SetMemoryHigh: setMemoryHigh,
	}
	res, err := reconcile.Once(ctx, rcDeps)
	metrics.ReconcilerRuns.Inc()
	metrics.ReconcilerLastDuration.Set(res.Duration.Seconds())
	for kind, n := range res.Orphans {
		metrics.ReconcilerOrphans.WithLabelValues(kind).Set(float64(n))
	}
	if err != nil {
		log.Error("startup: reconcile failed (continuing)", "err", err.Error())
	} else {
		log.Info("startup: reconcile complete",
			"rows", res.Rows,
			"reapplied", res.Reapplied,
			"stopped", res.Stopped,
			"errored", res.Errored,
			"egress_added", res.EgressAdded,
			"orphan_containers", res.Orphans["container"],
			"orphan_mounts", res.Orphans["mount"],
			"duration_ms", res.Duration.Milliseconds(),
		)
	}
	// Egress is disabled in the OSS build (egressMgr == nil); the
	// sources-active gauge is therefore always zero.
	metrics.EgressSourcesActive.Set(0)
	_ = metrics.RefreshSandboxGauge(ctx, st)

	// Phase 5 env knobs.
	idleThreshold := durationFromEnvSec("SANDBOXD_IDLE_THRESHOLD_SECONDS", defaultIdleThresholdSec)
	idleInterval := durationFromEnvSec("SANDBOXD_IDLE_REAP_INTERVAL_SECONDS", defaultIdleReapIntervalSec)
	pressureInterval := durationFromEnvSec("SANDBOXD_PRESSURE_INTERVAL_SECONDS", defaultPressureIntervalSec)
	headroomPct := floatFromEnv("SANDBOXD_MEM_HEADROOM_PCT", defaultMemHeadroomPct)
	refuseWakesPct := floatFromEnv("SANDBOXD_MEM_REFUSE_WAKES_PCT", defaultMemRefusePct)
	emergencyPct := floatFromEnv("SANDBOXD_MEM_EMERGENCY_PCT", defaultMemEmergencyPct)
	wakeCostMB := uintFromEnv("SANDBOXD_WAKE_COST_MB", defaultWakeCostMB)
	wakeTCPReady := durationFromEnvSec("SANDBOXD_WAKE_TCP_READY_TIMEOUT_SECONDS", defaultWakeTCPReadySec)
	wakeGrace := durationFromEnvSec("SANDBOXD_WAKE_GRACE_SECONDS", defaultWakeGraceSec)
	keepaliveMax := durationFromEnvSec("SANDBOXD_KEEPALIVE_MAX_SECONDS", defaultKeepaliveMaxSec)
	// The deployment value seeds the persisted global provider and remains the
	// fallback for databases created before the provider setting existed.
	defaultAgent := envDefault("SANDBOXD_DEFAULT_AGENT", "opencode")

	// Runtime-editable settings start from deployment defaults, then overlay
	// persisted edits (PATCH /v1/settings) so they survive restart.
	live := instancecfg.New(instancecfg.Snapshot{
		IdleEnabled:          idleInterval > 0,
		IdleThresholdSeconds: int(idleThreshold.Seconds()),
		KeepaliveMaxSeconds:  int(keepaliveMax.Seconds()),
		AgentProvider:        defaultAgent,
	})
	if persisted, perr := st.GetInstanceSettings(ctx); perr == nil {
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
		log.Info("instance settings: loaded persisted editable settings")
	}

	inflight := activity.NewInflightExec()
	refused := &atomic.Bool{}

	// Pressure reaper — long-lived. Also used synchronously by the
	// wake admission code, so we instantiate it before the wake
	// handler.
	pressure := &reaper.Pressure{
		Cfg: reaper.PressureConfig{
			Interval:       pressureInterval,
			HeadroomPct:    headroomPct,
			RefuseWakesPct: refuseWakesPct,
			EmergencyPct:   emergencyPct,
		},
		Store:    st,
		Docker:   dockerClient,
		Inflight: inflight,
		Egress:   egressMgr,
		Refused:  refused,
		Log:      log.With("component", "pressure-reaper"),
	}

	admitCfg := wake.AdmitConfig{
		WakeCostMB: wakeCostMB,
		FloorPct:   refuseWakesPct,
		Refused:    refused,
		Tick:       pressure.Tick,
	}

	wakeHandler, err := wake.New(
		st, dockerClient, domain,
		wake.Config{
			TCPReadyTimeout: wakeTCPReady,
			RefreshSeconds:  2,
		},
		admitCfg,
		egressMgr,
		idLocks,
		log.With("component", "wake"),
	)
	if err != nil {
		log.Error("startup: wake handler init failed", "err", err.Error())
		os.Exit(1)
	}
	// Phase 8 — gate stopped private-sandbox wakes through the same
	// preview-token check as /forward-auth.
	wakeHandler.Auth = authMw
	wakeHandler.Audit = auditLog
	wakeHandler.ForwardAuthDenyMode = denyMode
	wakeHandler.SetMemoryHigh = setMemoryHigh

	// app_config/secrets encryption: SANDBOXD_SECRETS_KEY (base64 32
	// bytes) or an auto-generated 0600 keyfile under the data dir.
	secretsCipher, err := secrets.Load(os.Getenv("SANDBOXD_SECRETS_KEY"), filepath.Join(dataDir, "secrets.key"))
	if err != nil {
		log.Error("init secrets encryption", "err", err.Error())
		os.Exit(1)
	}

	// GitHub Copilot is a hosted provider: its fine-grained PAT and the official SDK
	// runtime live only under this control-plane state directory. Sandboxes get
	// a private bridge URL plus a one-time task capability, never credentials.
	copilotManager, err := copilot.New(copilot.Config{
		StateDir: filepath.Join(stateDir, "copilot"),
		Cipher:   secretsCipher,
		Executor: copilotDockerExecutor{client: dockerClient, store: st},
		Log:      log.With("component", "copilot"),
	})
	if err != nil {
		log.Error("init GitHub Copilot provider", "err", err.Error())
		os.Exit(1)
	}
	copilotBridgeURL := envDefault("SANDBOXD_COPILOT_BRIDGE_URL", "http://sandboxd:9200")
	if err := validateCopilotBridgeURL(copilotBridgeURL); err != nil {
		log.Error("invalid GitHub Copilot bridge URL", "err", err.Error())
		os.Exit(1)
	}
	copilotBridgeAddr := envDefault("SANDBOXD_COPILOT_BRIDGE_ADDR", "0.0.0.0:9200")
	copilotBridgeListener, err := net.Listen("tcp", copilotBridgeAddr)
	if err != nil {
		log.Error("GitHub Copilot bridge listen", "addr", copilotBridgeAddr, "err", err.Error())
		os.Exit(1)
	}
	copilotBridgeServer := newCopilotBridgeServer(copilotManager.Handler())
	log.Info("GitHub Copilot bridge listening", "addr", copilotBridgeAddr)

	// Phase 10B A0 — host-side agent auth store (read-only here). Best-effort
	// root creation; never fatal.
	agentAuth := agentauth.NewStore(dataDir)
	if err := agentAuth.EnsureRoot(); err != nil {
		log.Warn("agent-auth: could not create store root", "err", err.Error())
	}
	// Credential-injecting auth proxy for claude-code (internal/authproxy). The
	// sandbox reaches it at SANDBOXD_AGENT_PROXY_URL (in-network name of THIS
	// process); we listen on SANDBOXD_AGENT_PROXY_ADDR. The real credential stays
	// here and is never mounted into the sandbox. Empty URL disables the proxy
	// (legacy mounted-credential behaviour).
	// Guided claude-code subscription login + token refresh (internal/agentauth).
	agentOAuth := agentauth.NewOAuth(agentAuth)
	if agentOAuth != nil {
		go func() {
			for {
				if err := agentOAuth.Refresh(); err != nil {
					log.Debug("claude oauth refresh", "err", err.Error())
				}
				time.Sleep(5 * time.Minute)
			}
		}()
	}

	agentProxyURL := envDefault("SANDBOXD_AGENT_PROXY_URL", "http://sandboxd:9100")
	if proxy := authproxy.New(agentAuth, log.With("component", "authproxy")); proxy != nil && agentProxyURL != "" {
		proxyAddr := envDefault("SANDBOXD_AGENT_PROXY_ADDR", "0.0.0.0:9100")
		go func() {
			log.Info("auth proxy listening", "addr", proxyAddr, "url", agentProxyURL)
			if err := (&http.Server{Addr: proxyAddr, Handler: proxy}).ListenAndServe(); err != nil {
				log.Error("auth proxy stopped", "err", err.Error())
			}
		}()
	} else {
		agentProxyURL = ""
	}

	// Update checker (best-effort): fetches the latest GitHub release, cached
	// ~6h, and surfaces update_available in GET /v1/settings. nil-safe in the
	// handler; a background goroutine below keeps its cache warm.
	updateChecker := &telemetry.Checker{}

	// gVisor (opt-in): SANDBOXD_RUNTIME=runsc runs sandboxes under runsc.
	// gVisor's netstack can't reach Docker's embedded DNS (127.0.0.11) on a
	// user-defined network, so we write a resolv.conf with real nameservers
	// (SANDBOXD_DNS, default public resolvers) and bind-mount it into each
	// sandbox. Requires runsc registered with `--host-uds=create` (docs/gvisor.md).
	sbxRuntime := envDefault("SANDBOXD_RUNTIME", "")
	var dnsResolvConf string
	if sbxRuntime == "runsc" {
		var ns []string
		for _, d := range strings.Split(os.Getenv("SANDBOXD_DNS"), ",") {
			if d = strings.TrimSpace(d); d != "" {
				ns = append(ns, d)
			}
		}
		if len(ns) == 0 {
			ns = []string{"1.1.1.1", "8.8.8.8"}
		}
		var b strings.Builder
		for _, n := range ns {
			fmt.Fprintf(&b, "nameserver %s\n", n)
		}
		dnsResolvConf = filepath.Join(dataDir, "gvisor-resolv.conf")
		if err := os.WriteFile(dnsResolvConf, []byte(b.String()), 0o644); err != nil {
			log.Error("gvisor: write resolv.conf failed; sandbox DNS may not resolve", "err", err.Error())
			dnsResolvConf = ""
		} else {
			log.Info("gvisor runtime enabled", "runtime", sbxRuntime, "dns", ns)
		}
		// gVisor sandboxes use public DNS (above) and cannot reach Docker's
		// embedded resolver. Pin each in-network control-plane service to its
		// IP before putting it in a sandbox environment.
		for _, service := range []struct {
			name  string
			value *string
		}{
			{name: "agent proxy", value: &agentProxyURL},
			{name: "GitHub Copilot bridge", value: &copilotBridgeURL},
		} {
			if *service.value == "" {
				continue
			}
			if u, perr := url.Parse(*service.value); perr == nil && u.Hostname() != "" && net.ParseIP(u.Hostname()) == nil {
				if ips, lerr := net.LookupHost(u.Hostname()); lerr == nil && len(ips) > 0 {
					if p := u.Port(); p != "" {
						u.Host = net.JoinHostPort(ips[0], p)
					} else {
						u.Host = ips[0]
					}
					log.Info("gvisor: pinned service to IP", "service", service.name, "from", *service.value, "to", u.String())
					*service.value = u.String()
				} else {
					log.Warn("gvisor: could not resolve service host to IP; sandboxes may fail to reach it", "service", service.name, "host", u.Hostname())
				}
			}
		}
	}

	// Self-healing start: rebuild a container that is missing, or that was
	// created from an older base image than we now run. The base image carries
	// runtimed (the in-sandbox supervisor), so without this an upgrade never
	// reaches long-lived sandboxes and they keep the old supervisor — and miss
	// new agent/model support. Lossless: everything durable is in the workspace
	// bind mount, and the container rootfs is read-only by construction.
	wakeHandler.Image = image
	wakeHandler.Recreate = func(ctx context.Context, sb *store.Sandbox) error {
		spec := sandboxspec.Build(sb, sandboxspec.Env{
			Image:             image,
			Network:           network,
			Userns:            userns,
			Runtime:           sbxRuntime,
			DNSResolvConf:     dnsResolvConf,
			PreviewDomain:     domain,
			PreviewEntrypoint: previewEntrypoint,
			PreviewTLS:        previewTLS,
			AgentProxyURL:     agentProxyURL,
			CopilotBridgeURL:  copilotBridgeURL,
			OpencodeModel:     envDefault("SANDBOXD_OPENCODE_MODEL", ""),
			OpencodeZenPath:   envDefault("SANDBOXD_OPENCODE_ZEN_PATH", ""),
			RuntimePreset:     runtimePresetForSandbox(ctx, st, sb),
		})
		// Remove any existing container first so the canonical name is free.
		// Retain the canonical fallback in case a manually recreated container
		// outlived the persisted container ID.
		containers := []string{sandboxname.Reference(sb.ID, sb.ContainerID.String)}
		if spec.Name != containers[0] {
			containers = append(containers, spec.Name)
		}
		for _, container := range containers {
			if err := dockerClient.Remove(ctx, container); err != nil && err != docker.ErrNotFound {
				log.Warn("recreate: remove existing container failed (continuing)", "err", err.Error())
			}
		}
		_, err := dockerClient.Run(ctx, spec)
		return err
	}

	server := &api.Server{
		Store:             st,
		Secrets:           secretsCipher,
		Update:            updateChecker,
		AgentAuth:         agentAuth,
		AgentOAuth:        agentOAuth,
		Copilot:           copilotManager,
		CopilotBridgeURL:  copilotBridgeURL,
		OpencodeModel:     envDefault("SANDBOXD_OPENCODE_MODEL", ""),
		OpencodeZenPath:   envDefault("SANDBOXD_OPENCODE_ZEN_PATH", ""),
		DefaultAgent:      defaultAgent,
		AgentProxyURL:     agentProxyURL,
		Docker:            dockerClient,
		Loopback:          loopMgr,
		Log:               log.With("component", "api"),
		PreviewDomain:     domain,
		Image:             image,
		Network:           network,
		Userns:            userns,
		Runtime:           sbxRuntime,
		DNSResolvConf:     dnsResolvConf,
		PreviewEntrypoint: previewEntrypoint,
		PreviewTLS:        previewTLS,
		PreviewURLScheme:  previewURLScheme,
		Upgrade: &upgrade.Manager{
			Docker: dockerClient, DataDir: dataDir, Version: version,
			SrcDir:        os.Getenv("SANDBOXD_SRC_DIR"),
			UpgraderImage: envDefault("SANDBOXD_UPGRADER_IMAGE", "sandboxd-upgrader:"+version),
			ReleaseExists: releaseExists,
		},
		PublicHTTPPort:      publicHTTPPort,
		SetMemoryHigh:       setMemoryHigh,
		Inflight:            inflight,
		Wake:                wakeHandler,
		Admit:               admitCfg,
		KeepaliveMax:        keepaliveMax,
		Egress:              egressMgr,
		Snapshot:            snapshotMgr,
		Locks:               idLocks,
		Auth:                authMw,
		Audit:               auditLog,
		Events:              eventRec,
		SnapshotsRoot:       snapshotsRoot,
		ForwardAuthDenyMode: denyMode,
		TemplatesDir:        envDefault("SANDBOXD_TEMPLATES_DIR", templatesRoot),
		LibraryRoot:         envDefault("SANDBOXD_LIBRARY_DIR", libraryRoot),
		LLMTxtPath:          envDefault("SANDBOXD_LLM_TXT_PATH", legacyAware("/etc/sandboxd/llm.txt", legacyEtcDir+"/llm.txt")),
		Instance: api.InstanceInfo{
			Version:              version,
			GitCommit:            gitCommit,
			AuthEnabled:          !authCfg.Disabled,
			StorageMode:          "directory", // OSS bind-mounted workspaces (see internal/loopback)
			EgressMode:           egressModeLabel(egressMgr),
			AgentProviders:       []string{"opencode", "claude-code", "codex", "github-copilot"},
			IdleReapEnabled:      idleInterval > 0,
			IdleThresholdSeconds: int(idleThreshold.Seconds()),
		},
		Live: live,
	}

	// Finalize any coding task left `running` by a previous sandboxd
	// run before the idle reaper (which trusts the task table) starts.
	server.ReconcileTasks(ctx)
	// Hosted Copilot turns have no runtimed event stream to reattach after a
	// restart, so make their interrupted callback state explicit before clients
	// can submit a response or a follow-up.
	server.RecoverConversations(ctx)

	// Phase 5 — after reconcile, if MemAvailable is
	// already below the healthy floor, run one synchronous pressure
	// tick before opening the listener. Keeps the host from
	// accepting requests while it's already saturated.
	if mi, err := reaper.ReadMemInfo(); err == nil {
		metrics.MemAvailablePercent.Set(mi.AvailablePct())
		metrics.MemAvailableBytes.Set(float64(mi.AvailableBytes()))
		if mi.AvailablePct() < headroomPct {
			log.Warn("startup: MemAvailable below headroom — running pressure tick before listener opens",
				"avail_pct", mi.AvailablePct())
			pressure.Tick(ctx)
		}
	}

	// Mux + middleware: catch-all dispatch in front of the API mux so
	// the loopback-only listener handles both the loopback API and
	// the Traefik catch-all reverse-proxied traffic. The Host header
	// is the only discriminator.
	// Phase 8 — the service-token auth middleware wraps the API mux
	// only. The local browser preview path remains the Docker wake route.
	// Entra production instead reaches the fail-closed control-plane preview
	// gateway before any runtime wake is requested.
	apiMux := authMw.Wrap(server.Handler())
	root := hostDispatch(wakeHandler, server.ProductionPreviewHandler(), server.IsPreviewHost, apiMux, log)
	root = logging.Middleware(log, root)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// --- Phase 5 background goroutines ---------------------------------
	gctx, gcancel := context.WithCancel(context.Background())
	defer gcancel()

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-gctx.Done():
				return
			case <-ticker.C:
				copilotManager.CleanupExpired()
			}
		}
	}()

	// --- Anonymous telemetry + update check (internal/telemetry) -------
	// Keep the release-checker cache warm regardless of the telemetry opt-out:
	// GET /v1/settings surfaces update_available from it, and no data leaves the
	// host until a client asks. A single fetch every 6h; best-effort.
	go func() {
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			if _, _, err := updateChecker.Latest(gctx); err != nil {
				log.Debug("update check failed (ignored)", "err", err.Error())
			}
			select {
			case <-gctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	// Anonymous usage heartbeat. ON by default; disabled via SANDBOXD_TELEMETRY=off
	// or DO_NOT_TRACK=1. The reporter only ever sends a random instance UUID, the
	// version, GOOS/GOARCH, a coarse sandbox-count bucket, and two feature flags —
	// no hostnames, IPs, paths, or user content (see docs/telemetry.md).
	if telemetry.EnabledFromEnv(os.Getenv) {
		instanceID, isNew, idErr := telemetry.InstanceID(filepath.Join(stateDir, "instance-id"))
		if idErr != nil {
			log.Warn("telemetry: could not establish instance id — telemetry disabled", "err", idErr.Error())
		} else {
			log.Info("telemetry: anonymous version + daily heartbeat enabled (no code/PII); disable with SANDBOXD_TELEMETRY=off")
			reporter := &telemetry.Reporter{
				InstanceID: instanceID,
				Version:    version,
				Arch:       runtime.GOARCH,
				OS:         runtime.GOOS,
				NewInstall: isNew,
				Send: telemetry.PostHogSend(
					envDefault("SANDBOXD_POSTHOG_HOST", telemetry.DefaultPostHogHost),
					envDefault("SANDBOXD_POSTHOG_KEY", telemetry.DefaultPostHogKey),
				),
				Snapshot: func() (int, bool, bool) {
					count := 0
					if list, err := st.List(gctx); err == nil {
						count = len(list)
					}
					consoleEnabled := false
					if h, err := st.GetPasswordHash(gctx); err == nil && h != "" {
						consoleEnabled = true
					}
					return count, !authCfg.Disabled, consoleEnabled
				},
				Log: log.With("component", "telemetry"),
			}
			go reporter.Run(gctx)
		}
	}

	// Idle reaper.
	idle := &reaper.Idle{
		Cfg: reaper.IdleConfig{
			Threshold: idleThreshold,
			Interval:  idleInterval,
			WakeGrace: wakeGrace,
		},
		Store:    st,
		Docker:   dockerClient,
		Inflight: inflight,
		Egress:   egressMgr,
		Log:      log.With("component", "idle-reaper"),
		// Phase 8B — hot-read the runtime-editable threshold + enable toggle.
		ThresholdFn: live.IdleThreshold,
		EnabledFn:   live.IdleEnabled,
	}
	go func() {
		if err := idle.Run(gctx); err != nil {
			log.Warn("idle reaper exited", "err", err.Error())
		}
	}()

	// Pressure reaper.
	go func() {
		if err := pressure.Run(gctx); err != nil {
			log.Warn("pressure reaper exited", "err", err.Error())
		}
	}()

	// Access-log tailer.
	tailer := &activity.Tailer{
		LogPath:        envDefault("SANDBOXD_ACCESS_LOG", accessLogPath),
		CheckpointPath: envDefault("SANDBOXD_TAILER_OFFSET", tailerOffsetFs),
		PreviewDomain:  domain,
		Store:          st,
		Log:            log.With("component", "access-log-tailer"),
	}
	go func() {
		if err := tailer.Run(gctx); err != nil {
			log.Warn("access-log tailer exited", "err", err.Error())
		}
	}()

	// Open-connection poller — only if the operator has supplied a
	// verified metric name via env. Without the env, the poller logs
	// "fallback mode" and returns; the access-log tailer alone
	// drives last_active_at and the wider WakeGrace window
	// compensates for long-lived WS quiet periods.
	pollerMetric := os.Getenv("SANDBOXD_POLLER_METRIC_RE")
	pollerLabel := envDefault("SANDBOXD_POLLER_SERVICE_LABEL", "service")
	pollerURL := envDefault("SANDBOXD_POLLER_URL", "http://127.0.0.1:8082/metrics")
	pollerInterval := durationFromEnvSec("SANDBOXD_POLLER_INTERVAL_SECONDS", 15)
	var pollerRE *regexp.Regexp
	if pollerMetric != "" {
		var perr error
		pollerRE, perr = regexp.Compile(pollerMetric)
		if perr != nil {
			log.Warn("startup: SANDBOXD_POLLER_METRIC_RE invalid — running poller in fallback",
				"err", perr.Error())
			pollerRE = nil
		}
	}
	poller := &activity.Poller{
		MetricsURL:    pollerURL,
		Interval:      pollerInterval,
		MetricNameRE:  pollerRE,
		ServiceLabel:  pollerLabel,
		PreviewDomain: domain,
		Store:         st,
		Log:           log.With("component", "connection-poller"),
	}
	go func() {
		if err := poller.Run(gctx); err != nil {
			log.Warn("connection poller exited", "err", err.Error())
		}
	}()

	// --- Egress goroutines: DISABLED in the OSS build -----------------
	// Earlier deployments ran a journald-tail egress collector, an
	// nftables drop-counter poller, and a systemd refresh-job watcher.
	// All three depend on host nftables / journald / systemd, which a
	// portable docker-compose deployment does not provide. With
	// egressMgr == nil there is nothing to collect, so these goroutines
	// are intentionally not started.
	_ = egressMgr // documents the deliberate nil

	// --- nginx registry-proxy hot-reloader: DISABLED in the OSS build -
	// Earlier deployments ran a single host-side nginx caching proxy
	// for npm/pypi/crates/bun and hot-reloaded it on config change. The
	// OSS image points package managers at the public registries
	// directly, so there is no proxy container to watch. Re-enable by
	// running your own proxy and setting SANDBOXD_NGINX_WATCH_PATHS +
	// SANDBOXD_NGINX_CONTAINER (the watcher code is retained).
	_ = nginxwatch.ExecResult{} // keep the import referenced

	// --- Auto-snapshotter: DISABLED in the OSS build ------------------
	// The hourly auto-snapshotter zstd-compresses each sandbox's
	// workspace image. The portable build stores workspaces as plain
	// directories (not loopback .img files), so the compress path does
	// not apply. The manual snapshot/template REST endpoints remain
	// wired but are EXPERIMENTAL on directory storage (see README).
	_ = snapshotMgr // constructed for the reconciler debris sweep + API

	// --- Phase 8 workspace_owner orphan check -------------------------
	// Every 6 h, log workspace_owner rows whose .img is
	// gone. Never deletes; disposition is the operator's.
	go func() {
		ownerLog := log.With("component", "owner-orphan-check")
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-gctx.Done():
				return
			case <-t.C:
				if n := reconcile.CheckWorkspaceOwnerOrphans(gctx, st, loopMgr, ownerLog); n > 0 {
					ownerLog.Warn("workspace_owner rows with no .img on disk", "count", n)
				}
			}
		}
	}()

	// Listen + serve.
	errCh := make(chan error, 1)
	if copilotBridgeServer != nil {
		go func() {
			err := copilotBridgeServer.Serve(copilotBridgeListener)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("GitHub Copilot bridge: %w", err)
			}
		}()
	}
	go func() {
		log.Info("startup: listening",
			"addr", addr,
			"preview_domain", domain,
			"image", image,
			"idle_threshold", idleThreshold.String(),
			"idle_interval", idleInterval.String(),
			"pressure_interval", pressureInterval.String(),
			"mem_headroom_pct", headroomPct,
			"mem_refuse_wakes_pct", refuseWakesPct,
			"mem_emergency_pct", emergencyPct,
			"wake_cost_mb", wakeCostMB,
			"wake_tcp_ready", wakeTCPReady.String(),
			"wake_grace", wakeGrace.String(),
			"poller_mode", pollerModeLabel(pollerRE),
		)
		err := httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Block until a terminating signal or server-error. SIGHUP is not
	// terminating — it re-reads the auth config (token rotation) and
	// the loop continues. SIGINT / SIGTERM break out to shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				log.Info("reload: SIGHUP — re-reading auth config", "env_file", envFile)
				if env, err := auth.LoadEnvFile(envFile); err != nil {
					log.Error("reload: read env file failed (keeping current config)",
						"err", err.Error())
				} else {
					nc := auth.ParseConfig(auth.MapGetter(env))
					currentAuth := authMw.Snapshot()
					if err := validateAuthProfileReload(currentAuth, nc); err != nil {
						log.Error("reload: rejected authentication profile change; restart required",
							"current_profile", authProfile(currentAuth),
							"requested_profile", authProfile(nc))
						continue
					}
					if nc.Profile == auth.ProfileEntra && storeConfig.Profile != store.ProfileProduction {
						log.Error("reload: rejected production authentication without PostgreSQL persistence")
						continue
					}
					if nc.Profile == auth.ProfileEntra && !nc.ProductionReady() {
						log.Error("reload: rejected incomplete production authentication configuration")
						continue
					}
					authMw.Reload(nc)
					log.Info("reload: auth config reloaded",
						"api_tokens", len(nc.APITokens),
						"preview_secrets", len(nc.PreviewSecrets),
						"auth_disabled", nc.Disabled)
				}
				continue
			}
			log.Info("shutdown: signal received", "signal", sig.String())
		case err := <-errCh:
			log.Error("shutdown: server error", "err", err.Error())
		}
		break
	}

	gcancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		log.Error("shutdown: http server shutdown failed", "err", err.Error())
	}
	if copilotBridgeServer != nil {
		if err := copilotBridgeServer.Shutdown(shutCtx); err != nil {
			log.Error("shutdown: GitHub Copilot bridge shutdown failed", "err", err.Error())
		}
	}
}

// hostDispatch routes incoming requests to either the wake catch-all
// (when Host header matches s-<id>-<port>.preview.<domain>) or the
// loopback API. Both share the same listener so the operator only
// has to wire one entry into Traefik's file provider.
func hostDispatch(w *wake.Handler, productionPreview http.Handler, productionHostMatches func(string) bool, apiMux http.Handler, _ any) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if productionPreview != nil && productionHostMatches != nil && productionHostMatches(r.Host) {
			productionPreview.ServeHTTP(rw, r)
			return
		}
		if w != nil && w.HostMatchesPreview(r.Host) {
			w.ServeCatchAll(rw, r)
			return
		}
		apiMux.ServeHTTP(rw, r)
	})
}

func envDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// envSplit returns the env var split by `sep`, trimming empty entries.
// Falls back to `def` (also split) if unset. Returns nil if the
// resulting list is empty (i.e. operator deliberately disabled).
func envSplit(k, def, sep string) []string {
	raw := os.Getenv(k)
	if raw == "" {
		raw = def
	}
	if raw == "" {
		return nil
	}
	parts := []string{}
	for _, p := range splitNonEmpty(raw, sep) {
		parts = append(parts, p)
	}
	return parts
}

func splitNonEmpty(s, sep string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if string(r) == sep {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// dockerExecAdapter bridges *docker.Client to nginxwatch.Execer
// (same shape, different ExecResult type, avoiding an import cycle if
// docker grew nginx-specific types later).
type dockerExecAdapter struct{ d *docker.Client }

func (a dockerExecAdapter) Exec(ctx context.Context, name string, cmd []string) (nginxwatch.ExecResult, error) {
	r, err := a.d.Exec(ctx, name, cmd)
	return nginxwatch.ExecResult{Stdout: r.Stdout, Stderr: r.Stderr, ExitCode: r.ExitCode}, err
}

// copilotDockerExecutor constrains SDK tools to the current sandbox namespace,
// unprivileged user, and application workspace before invoking docker exec.
type copilotDockerExecutor struct {
	client *docker.Client
	store  *store.Store
}

func (a copilotDockerExecutor) ExecScoped(ctx context.Context, request copilot.ScopedExecRequest) (copilot.ScopedExecResult, error) {
	if a.client == nil || a.store == nil {
		return copilot.ScopedExecResult{}, errors.New("GitHub Copilot executor is unavailable")
	}
	sandboxID, childID, isChild, validTarget := copilot.WorkspaceToolContainerTarget(request.Container)
	if !validTarget ||
		request.User != "sandbox" || request.Workdir != "/home/sandbox/workspace/app" {
		return copilot.ScopedExecResult{}, errors.New("invalid GitHub Copilot sandbox scope")
	}
	container := request.Container
	if isChild {
		childID = strings.ToUpper(childID)
		child, err := a.store.GetConversationChild(ctx, childID)
		if err != nil || child.Status != store.ConversationChildRunning ||
			child.WorkerContainer != request.Container {
			return copilot.ScopedExecResult{}, errors.New("GitHub Copilot worker target is unavailable")
		}
	} else {
		sandboxID = strings.ToUpper(sandboxID)
		sandbox, err := a.store.Get(ctx, sandboxID)
		if err != nil || sandbox.Status != "running" || sandboxname.Container(sandbox.ID) != request.Container {
			return copilot.ScopedExecResult{}, errors.New("GitHub Copilot sandbox target is unavailable")
		}
		container = sandboxname.Reference(sandbox.ID, sandbox.ContainerID.String)
	}
	result, err := a.client.ExecScoped(ctx, docker.ScopedExecRequest{
		Container:   container,
		User:        request.User,
		Workdir:     request.Workdir,
		Command:     request.Command,
		Stdin:       request.Stdin,
		Timeout:     request.Timeout,
		OutputLimit: request.OutputLimit,
	})
	return copilot.ScopedExecResult{
		Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode,
	}, err
}

func newCopilotBridgeServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		// A sandbox must not be able to occupy a bridge handler indefinitely
		// by sending a valid header and then trickling the request body.
		ReadTimeout: 15 * time.Second,
		IdleTimeout: 60 * time.Second,
	}
}

func validateCopilotBridgeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil ||
		(u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" ||
		u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("must be an http(s) URL with host and no credentials, path, query, or fragment")
	}
	return nil
}

func durationFromEnvSec(k string, dSec int) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return time.Duration(dSec) * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return time.Duration(dSec) * time.Second
	}
	if n < 0 {
		n = 0
	}
	return time.Duration(n) * time.Second
}

func floatFromEnv(k string, d float64) float64 {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return d
	}
	return f
}

func uintFromEnv(k string, d uint64) uint64 {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return d
	}
	return n
}

func intFromEnv(k string, d int) int {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return d
	}
	return n
}

// boolFromEnv parses a boolean env var. Accepts 1/true/yes/on (any
// case) as true and 0/false/no/off as false; anything else (including
// unset) returns the default.
func boolFromEnv(k string, d bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return d
	}
}

func pollerModeLabel(re *regexp.Regexp) string {
	if re == nil {
		return "fallback"
	}
	return "active"
}

// egressModeLabel is the safe egress mode string for GET /v1/settings. A nil
// manager (the OSS default) means no egress policy is enforced.
func egressModeLabel(m *egress.Manager) string {
	if m == nil {
		return "disabled"
	}
	return "enabled"
}

// buildVersion and buildCommit are stamped at build time via
//
//	-ldflags "-X main.buildVersion=… -X main.buildCommit=…"
//
// (see control-plane/Dockerfile + docker-compose build args, fed by
// upgrade.sh/install.sh from `git describe`). When unset — e.g. a bare
// `go build` — buildIdent falls back to the module's VCS build info, then to
// dev/unknown. This is what `sandboxd version`, /v1/settings, and the telemetry
// heartbeat report, so the version-distribution insight is only meaningful once
// these are stamped.
var (
	buildVersion string
	buildCommit  string
)

func buildIdent() (version, gitCommit string) {
	version = buildVersion
	gitCommit = buildCommit
	if version == "" || gitCommit == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				if s.Key == "vcs.revision" && s.Value != "" && gitCommit == "" {
					gitCommit = s.Value
				}
			}
			if info.Main.Version != "" && info.Main.Version != "(devel)" && version == "" {
				version = info.Main.Version
			}
		}
	}
	if version == "" {
		version = "dev"
	}
	if gitCommit == "" {
		gitCommit = "unknown"
	}
	if len(gitCommit) > 12 {
		gitCommit = gitCommit[:12]
	}
	return version, gitCommit
}

// runtimePresetForSandbox returns the owning app's runtime preset, so a
// recreated container gets the same RUNTIMED_RUNTIME_PRESET it was created
// with. Best-effort: an unknown app just means runtimed uses its default.
func runtimePresetForSandbox(ctx context.Context, st *store.Store, sb *store.Sandbox) string {
	if sb == nil || !sb.AppID.Valid || sb.AppID.String == "" {
		return ""
	}
	app, err := st.GetApp(ctx, sb.AppID.String)
	if err != nil || app == nil || !app.RuntimePreset.Valid {
		return ""
	}
	return app.RuntimePreset.String
}

// legacyAware returns preferred unless it does not exist and the pre-rename
// path does — the "sandboxed" → "sandboxd" rename must never strand an
// existing install's files.
func legacyAware(preferred, legacy string) string {
	if _, err := os.Stat(preferred); err == nil {
		return preferred
	}
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return preferred
}

// releaseExists reports whether tag is a published GitHub release — the only
// targets the console may upgrade to. Best-effort: network failure = false.
func releaseExists(ctx context.Context, tag string) bool {
	req, err := http.NewRequestWithContext(ctx,
		"GET", "https://api.github.com/repos/tastyeffectco/sandboxd/releases/tags/"+tag, nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
