# sandboxd — control plane

The Go control plane for [sandboxd](../README.md). In the default local profile,
a single binary drives the Docker daemon (via the `docker` CLI), stores state in
SQLite, runs the idle and pressure reapers, and serves the HTTP API and wake
path. The production profile runs the control plane on AKS, uses Kubernetes for
sandbox runtime operations, and stores durable state in PostgreSQL; see
[`../docs/production-safety.md`](../docs/production-safety.md).

See [`../ARCHITECTURE.md`](../ARCHITECTURE.md) for the full design.

## Build & test

```bash
go build ./...
go test ./...
go vet ./...
```

`sandboxd` uses cgo (mattn/go-sqlite3); the container build (`Dockerfile`) sets
`CGO_ENABLED=1`. `runtimed` (`cmd/runtimed`) is CGO-free and is compiled into
the sandbox base image instead.

## Packages

| Package | Responsibility |
|---|---|
| `cmd/sandboxd` | daemon entrypoint, env wiring, background goroutines |
| `cmd/runtimed` | in-sandbox supervisor (baked into the base image) |
| `internal/docker` | thin typed wrapper over the `docker` CLI |
| `internal/loopback` | per-sandbox workspace storage (directory-backed) |
| `internal/traefik` | preview-route label generation |
| `internal/reaper` | idle (stop-on-idle) + host-memory pressure reapers |
| `internal/wake` | wake-on-request handler + warming page |
| `internal/reconcile` | boot-time convergence of Docker → SQLite |
| `internal/store` | Local SQLite access + numbered migrations; production uses PostgreSQL |
| `internal/api` | HTTP handlers (`/sandbox*`, `/v1/*`, wake, forward-auth) |
| `internal/auth` | Profile-specific local or Entra/session and preview authorization |

## Configuration

All runtime configuration is via environment variables, set by the compose file
from `../.env`. The ones the OSS build adds or changes:

| Variable | Default | Purpose |
|---|---|---|
| `PREVIEW_DOMAIN` | `localhost` | domain preview URLs hang off |
| `PREVIEW_ENTRYPOINT` | `web` | Traefik entrypoint on preview routers |
| `PREVIEW_TLS` | `false` | emit `tls=true` on preview routers |
| `SANDBOXD_NETWORK` | `sandboxd_net` | docker network sandboxes join |
| `SANDBOXD_USERNS` | `host` | `--userns` for sandboxes + the seed container |
| `SANDBOXD_DATA_DIR` | `/var/lib/sandboxd` | workspaces + SQLite + logs |
| `SANDBOXD_SET_MEMORY_HIGH` | `false` | write cgroup `memory.high` (needs host cgroup access) |
| `SANDBOXD_IMAGE` | `sandboxd-base:0.3.0` | per-sandbox base image |
| `SANDBOXD_API_AUTH_DISABLED` | `true` | local profile only; open API for loopback use |
| `SANDBOXD_API_TOKENS` | — | local `name:secret` service-token fallback |
| `SANDBOXD_IDLE_THRESHOLD_SECONDS` | `2100` | idle window before `docker stop` |

The local password-backed console session and bearer API tokens remain supported
for trusted, single-host use. Production does not use either mechanism:
authentication is OIDC authorization-code PKCE with server-side sessions in one
Entra tenant, with `sandboxd.user` and `sandboxd.admin` roles and immutable OID
ownership. Production secrets come from Key Vault through workload identity;
they are not configured as sandbox environment variables.

## Local Kind runtime

`SANDBOXD_PLATFORM=kubernetes-local` is a development and integration-test
profile for a preconfigured Kind cluster. It requires SQLite, exactly one
control-plane replica, local email/password accounts, `PREVIEW_DOMAIN=localhost`,
HTTP on port `9090`, the `standard` StorageClass, and no RuntimeClass. Sandboxes
use a PVC-backed workspace and a local-only permissions init container so a
retained local-path volume is writable by the unprivileged runtime user.

It intentionally omits Entra, PostgreSQL, Key Vault, HTTPS, and Kata isolation;
those remain mandatory for `SANDBOXD_PLATFORM=kubernetes`. Build, deploy, verify,
and remove the local profile with the scripts documented in
[`../infra/dev-kind/README.md`](../infra/dev-kind/README.md).

## Production runtime

Production deployment is a separately gated AKS profile. Kata Pod Sandboxing
runs each workload on supported Azure Linux/Gen2 nodes; Azure Disk PVCs and
VolumeSnapshots hold workspace state, PostgreSQL Flexible Server holds control
plane state, and Azure CNI Cilium enforces default-deny networking. Sandboxes
may use public HTTPS DNS egress only; Azure metadata and internal/private
network access are blocked. Provider credentials never enter sandboxes, and
hosted GitHub Copilot stays in the trusted control plane.

The `infra/` Bicep/Kubernetes/Azure DevOps assets describe deployment
prerequisites and parameters in [`../infra/README.md`](../infra/README.md).
They are not invoked by this package or by local Compose, and no Azure resources
are assumed to be provisioned. Local SQLite/directory data is not migrated
automatically.

## API sketch

```
POST   /sandbox                      create (body: {"ports":[...]}; id optional)
GET    /sandboxes                    list
GET    /sandbox/{id}                 get
POST   /sandbox/{id}/exec            run a command (non-interactive)
DELETE /sandbox/{id}                 destroy container (workspace kept)
POST   /sandbox/{id}/purge           destroy + delete workspace
POST   /v1/sandboxes/{id}/stop       stop (idle); wakes on next preview hit
POST   /v1/sandboxes/{id}/tasks      submit a coding task to runtimed
PUT    /v1/sandboxes/{id}/files      write files into the workspace
GET    /healthz  GET /readyz         liveness / readiness
```
