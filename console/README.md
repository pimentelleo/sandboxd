# sandboxd console

An optional web console for managing **apps** on top of sandboxd — see your
apps, open one, view its live preview, submit agent tasks, watch task logs, and
start/stop/snapshot its sandbox.

A Vite + React SPA. It talks **only** to the public sandboxd `/v1` API
(`docs/openapi.yaml`) — no Go imports, no database, no workspace access. That
boundary is deliberate: once `/v1` stabilizes, this folder splits cleanly into a
standalone `sandboxd-console` repo.

## Run it (console mode)

From the repo root:

```bash
docker compose --profile console up
```

Then open <http://console.localhost> (or `console.<PREVIEW_DOMAIN>:<HTTP_PORT>`).
Core mode (`docker compose up`, no profile) runs sandboxd without the console.

The console is routed through the **same Traefik as the previews**, by Host
header — `console.<domain>` → console, `*.preview.<domain>` → sandboxes — so it
shares one entrypoint. nginx serves the built SPA and proxies `/v1` to the
control-plane service on the internal network, so the browser uses same-origin
relative paths: no CORS. The local image targets Docker's `sandboxd` service;
the separate unprivileged production image targets the AKS ClusterIP service.
The local single-user default has no auth; plain HTTP is suitable only for local
use.

## Develop

```bash
pnpm install
pnpm dev            # http://127.0.0.1:8787, proxies /v1 to $SANDBOXD_URL (default 127.0.0.1:9090)
pnpm build
pnpm test:e2e       # Playwright — needs the stack up (see above)
```

## App detail screen

Per app: live **Preview / endpoint** (worker-only apps show endpoint `none`,
which is valid), agent chat, **start/stop**, **Config & Secrets** (sensitive
values are write-only — set once, never shown),
**Snapshots** (capture, plus confirm-gated restore/fork), an **Activity**
timeline (durable app events, newest-first), and a **Processes** panel
(name/kind/running/pid/restarts with per-process recent logs).

When **GitHub Copilot** is selected, the chat is a durable conversation rather
than the one-shot task UI used by other agents. It renders streaming replies,
queued messages, native questions, plan approvals, cancellation, and a
new-conversation control once idle. `/plan`, `/interactive`, and `/autopilot`
select the corresponding turn mode; the selector stays in sync with those
commands. The transcript and interaction cards survive a page reload or a
sandbox sleep, and the console reconnects to the redacted SSE stream from its
durable event cursor.

The Copilot composer loads the connected account's safe model catalog and lets
the user choose a model, one of its supported reasoning efforts, and standard
or long context for each message. A message can still use the sandboxd/Copilot
default if catalog discovery is unavailable. The selected values are sent with
the message and are immutable once that turn enters the server-side FIFO queue.

Copilot delegated tasks appear inline with their inherited model settings,
status, result, and changed files. Opening a changed file fetches one bounded
review record on demand. Those changes remain in an isolated worker workspace:
the console can cancel or review them, but it never applies them automatically.

New App offers a **runtime preset** picker (React/Vite, Next.js, AdonisJS,
Node/Express, FastAPI, Worker), data-driven from `GET /v1/presets`; the chosen preset is stored
on the app and applied to its sandbox.

## Authentication and previews

The local Compose profile can remain auth-off on loopback, or use the existing
local password-backed browser session and bearer API-token mechanisms for a
trusted host. These are not production authentication options.

The production console is HTTPS-only at `console.<domain>` and uses OIDC
authorization-code PKCE against one Entra tenant. The control plane creates a
server-side session in a host-only cookie. Access is assigned through the
`sandboxd.user` and `sandboxd.admin` roles and ownership is keyed to the
immutable Entra OID; admins have full access. Preview URLs are served at
`*.preview.<domain>` over HTTPS. An owner or authorized admin obtains access
through a secure, one-time bootstrap ticket, then the browser uses a host-only
cookie. The console never receives provider credentials, and hosted GitHub
Copilot remains a trusted control-plane integration.

Production deployment is separately gated and uses the AKS/Kata profile. See
[`../docs/production-safety.md`](../docs/production-safety.md) and
[`../infra/README.md`](../infra/README.md) for the deployment contract.

## Scope (MVP)

The local UI remains single-user by default, with public previews. Production
adds multi-user Entra auth and owner/admin-authorized previews; do not infer
those guarantees when running the local profile.
