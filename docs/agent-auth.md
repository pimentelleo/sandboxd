# Agent auth & context

How sandboxd gives a sandbox's coding agent credentials and environment context
without putting secrets in the workspace, snapshots, container env, logs,
events, or task results.

sandboxd supports sandbox-resident **OpenCode** and **Claude Code** adapters,
plus hosted **GitHub Copilot** through the official
[`copilot-sdk`](https://github.com/github/copilot-sdk). Copilot's runtime and
GitHub fine-grained personal access token remain in the control plane; they are never installed,
mounted, or env-injected into a sandbox. **Codex** has a backend adapter but is
parked in the UI until its subscription authentication can be secured.

## Connecting a provider

`GET /v1/agents` lists providers with `installed_state`, `status`, `method`,
`supports_oauth`, `supports_api_key`, `supports_pat`, `runnable`, and `hosted`.

| Method | Endpoint | Notes |
| --- | --- | --- |
| **API key** | `POST /v1/agents/{provider}/api-key` `{"api_key":"..."}` | Stored opaquely and used proxy-side by default. |
| **Import** | `POST /v1/agents/{provider}/import` `{"credentials":"..."}` | Paste the bundle your local `<cli> login` produced. Written verbatim, never parsed. |
| **Guided OAuth** (Claude Code) | `POST /v1/agents/claude-code/oauth/start` then `.../oauth/finish` `{"code":"<code#state>"}` | A `claude setup-token` PKCE flow driven server-side. |
| **Fine-grained PAT** (GitHub Copilot) | `POST /v1/agents/github-copilot/pat` `{"token":"github_pat_..."}` | The control plane validates and encrypts the write-only token; it is never returned to the browser or sandbox. |

`POST /v1/agents/{provider}/disconnect` removes the provider credential.
Sandbox-resident credentials are opaque; GitHub Copilot credentials are AES-GCM
encrypted with sandboxd's secrets key. Neither form is returned, echoed, logged,
or emitted in events; console inputs are write-only.

## Credential delivery and hosted execution

### Sandbox-resident agents: credential-injecting proxy

When the auth proxy is enabled (`SANDBOXD_AGENT_PROXY_URL`, default
`http://sandboxd:9100`), sandbox-resident agents reach their model provider
through a reverse proxy in the control plane (`internal/authproxy`) that holds
the real credentials. No credential - API key or OAuth token - is mounted or
env-injected into the sandbox; the agent gets only a base URL and a dummy key,
and the proxy swaps in the real `Authorization` or `X-Api-Key` header on the
wire. A task can neither read, exfiltrate, nor clobber the credential.

Sandbox base URLs are `<proxy>/<agent>/<upstream>/...`; the proxy resolves the
agent's stored credential for that upstream and injects it. Upstreams:
`anthropic`, `openai`, `zen` (OpenCode Zen pay-as-you-go), `zengo` (OpenCode Zen
"go" subscription), and the MiniMax direct endpoints `minimax`, `minimax-cn`,
`minimax-anthropic`, and `minimax-anthropic-cn`.

- **Claude Code** uses `ANTHROPIC_BASE_URL =
  <proxy>/claude-code/anthropic` and `ANTHROPIC_API_KEY =
  sandboxd-proxy-injected`. It supports API keys and Claude subscriptions.
- **OpenCode** uses an `OPENCODE_CONFIG` file that runtimed writes. The config
  defines a custom OpenAI-compatible provider at the proxy's resolved IP, since
  the Zen provider ignores base-URL env vars and its Bun runtime rejects a bare
  hostname. The task model is rewritten to `proxy/<id>`.
- **Codex** is parked because its ChatGPT-subscription backend is a WebSocket
  that cannot yet be proxied.
- **MiniMax** is credential-only, not a task agent. Its connected API key is
  injected by the proxy for MiniMax direct upstreams. The OpenCode model picker
  exposes `MiniMax-M3` and `MiniMax-M2.7`; set
  `SANDBOXD_MINIMAX_REGION=cn_zh` to use the China endpoint.

  | Region | OpenAI-compatible | Anthropic-compatible |
  | --- | --- | --- |
  | `global_en` (default) | `https://api.minimax.io/v1` | `https://api.minimax.io/anthropic` |
  | `cn_zh` | `https://api.minimaxi.com/v1` | `https://api.minimaxi.com/anthropic` |

The real credential never appears in the sandbox filesystem or env. Verify with
`mount | grep agent-auth` (no mount) and `ls /run/agent-auth` (absent) while the
proxy is enabled.

### GitHub Copilot: official SDK hosted in the control plane

GitHub Copilot uses the official SDK in `internal/copilot`, rather than a
Copilot CLI or credential inside the sandbox. The SDK's bundled runtime is
included only in the control-plane image.

Create a fine-grained GitHub personal access token for the account that will
run Copilot tasks. Grant it the **Copilot Requests** permission. The account
must have an active Copilot entitlement. Classic PATs (`ghp_...`) are rejected;
the SDK supports fine-grained tokens (`github_pat_...`) only.

Connect it through the console's **Settings -> Agents** field, or through the
write-only API endpoint:

```bash
API=http://127.0.0.1:9090
read -rsp 'GitHub fine-grained PAT: ' COPILOT_PAT; echo
curl -s -XPOST "$API/v1/agents/github-copilot/pat" \
 -H 'content-type: application/json' -d "{\"token\":\"$COPILOT_PAT\"}"
unset COPILOT_PAT
```

The connection confirms the GitHub identity, then stores the token encrypted
with sandboxd's secrets key. It is not refreshed: when it expires or is revoked,
connect a replacement token. Existing Device Flow state from pre-PAT versions is
discarded on startup, requiring a fresh fine-grained token.

The legacy one-shot `POST /v1/sandboxes/{id}/tasks` Copilot path creates a
random, one-use capability bound to that sandbox and task and valid for two
minutes. `runtimed` exchanges it only with the private
`SANDBOXD_COPILOT_BRIDGE_URL`; the bridge is not published on a host port.

The console's GitHub Copilot path instead uses
`/v1/sandboxes/{id}/conversation`. It is a durable, per-sandbox conversation:
messages are queued FIFO, native questions and plan approvals are persisted
before display, and the console reconnects through a redacted SSE cursor. The
SDK session is keyed by opaque conversation ID in the control plane. A sandbox
may sleep while a question or plan is pending; it wakes before an accepted
response resumes workspace tools. A control-plane restart attempts to release
the prepared runtimed task record before recording an unrecoverable in-flight
provider call as interrupted and admitting subsequent work.

Both paths give the SDK only five custom tools: `list_files`, `read_file`,
`search_files`, `write_file`, and `run_command`. Each tool is executed through
a bounded `docker exec` as user `sandbox`, with workdir
`/home/sandbox/workspace/app`, no TTY, validated paths and arguments, a
deadline, and output caps. The SDK has config discovery, file hooks, host Git
operations, session store, and skills disabled.

For a conversation turn, `/plan` is read-only until the user explicitly
approves the provider's plan action. `/interactive` permits normal workspace
tools. `/autopilot` chooses a bounded least-surprising assumption for questions
and approves an unexpected plan exit as autopilot. Resetting an idle
conversation archives its transcript and starts a fresh one. Purging or
disconnecting removes retained session mappings and outstanding capabilities.
Connecting a different GitHub account also cancels prior active tasks before it
accepts the new credential.

### OpenCode Zen: subscription vs pay-as-you-go

OpenCode Zen has two gateways, and the same key behaves differently on each.
`SANDBOXD_OPENCODE_ZEN_PATH` selects the path OpenCode uses:

| Value | Endpoint | Billing | Models |
| --- | --- | --- | --- |
| `zen` (default) | `opencode.ai/zen/v1` | Pay-as-you-go; needs a positive wallet balance | Full catalog: `claude-*`, `gpt-*`, `gemini-*`, plus open models |
| `zengo` | `opencode.ai/zen/go/v1` | Included in the OpenCode "go" subscription | Open models only: GLM, Kimi, MiniMax, DeepSeek, Qwen, MiMo, and others |

Use `zengo` with the "go" subscription. Use `zen` when a paid Zen wallet should
provide Claude, GPT, or Gemini. `Insufficient balance` means `zen` has no
balance; `Model ... is not supported` means the selected model is not available
on the chosen path.

### OpenCode free tier: no key required

OpenCode works out of the box with no connected credential. Zen serves free
models with IDs ending in `-free`, such as `big-pickle`,
`deepseek-v4-flash-free`, and `mimo-v2.5-free`. With no OpenCode credential, the
proxy drops the sandbox's dummy key, injects nothing, and always uses the `zen`
endpoint even when the deployment configures `zengo`. The control plane defaults
to a free model on a fresh install.

A connected OpenCode key always wins and enables its paid catalog. This free
path is OpenCode-only. Other sandbox-resident agents require a connection;
GitHub Copilot is separately admitted by its hosted provider.

### Per-agent default model

Set a default model in **Settings -> AI Agents -> Default model** or through
`PATCH /v1/settings` `agents.default_models`. A task's model precedence is:

1. The task's own `model`.
2. The per-agent default.
3. The OpenCode free-tier default when OpenCode has no key.
4. The runtimed or agent default.

### Fallback: no proxy

If the proxy is disabled, a connected sandbox-resident provider's opaque auth
directory (`<DataDir>/agent-auth/<provider>/`, outside any workspace) is
bind-mounted read-only at `/run/agent-auth/<provider>` and runtimed points the
agent's `HOME` there. An API-key connection instead injects the provider's one
key var. This is the legacy path; GitHub Copilot never uses it.

## Env scrub

Sandbox-resident agent process env is scrubbed of secret-shaped variables
(`*_KEY`, `*_TOKEN`, `*_SECRET`, `*_PASSWORD`, `*_CREDENTIALS`, and runtimed's
own `RUNTIMED_*`). Therefore:

- An agent cannot pick up a stray credential from the container env.
- A key set via a sandbox's `env`, such as `ANTHROPIC_API_KEY`, does not reach
  the agent. Connect the provider instead.

Non-secret config (`PATH`, `HOME`, `LANG`, `*_MODEL`, `*_BASE_URL`, and similar)
is retained.

## Agent context: platform prompt and per-app guide

Two context layers are injected without changing app source:

- **Platform system prompt**:
  [`control-plane/internal/agentprompt/prompt.md`](../control-plane/internal/agentprompt/prompt.md)
  is embedded into the control plane and runtimed. It is rendered with the
  sandbox's real port and health path, then supplied to Claude Code through
  `--append-system-prompt`, prefixed for OpenCode, and sent to GitHub Copilot
  through the private bridge. It defines the guardrails: bind `0.0.0.0`, do not
  rewrite `sandbox.yaml` carelessly, do not touch `/run/agent-auth`, avoid
  destructive Git on the main branch, and verify on the loopback port. The prompt
  is read-only at `agents.system_prompt` in `GET /v1/settings`.
- **Per-app `AGENTS.md`**: detected stack, upstream repo link, and run guidance
  written to `workspace/app/AGENTS.md` on runtime apply unless the imported repo
  already provides one. This is console behavior on top of `/v1`, not a core
  write.

## What's guaranteed

Credentials are absent from the workspace, snapshots (workspace tree only),
container env / `docker inspect`, task results, events, and logs. Claude Code
subscription tokens live behind the control-plane proxy. GitHub Copilot PATs and
SDK runtime state remain in the control plane.

## Running a task

Submit `POST /v1/sandboxes/{id}/tasks` with
`{"prompt":"...","agent":"opencode","model":"opencode/glm-5"}`.
Sandbox-resident adapters run their CLI in the workspace
(`--dangerously-skip-permissions`, because the containment boundary is the
throwaway sandbox, not the agent). GitHub Copilot uses
`{"prompt":"...","agent":"github-copilot","model":"<SDK model id>"}` and streams
through the hosted SDK bridge. These are all one-shot task calls: events stream
over SSE (`/tasks/{taskId}/events`) and a final result ends the task.

For the conversational Copilot API, submit
`POST /v1/sandboxes/{id}/conversation/messages` with
`{"prompt":"...","mode":"interactive"}`. Modes are `interactive`, `plan`, and
`autopilot`; a leading `/interactive`, `/plan`, or `/autopilot` in the prompt
overrides the JSON mode. Get the initial transcript from
`GET /v1/sandboxes/{id}/conversation`, then connect
`GET /v1/sandboxes/{id}/conversation/events?after=<event_cursor>`. Answer
native input and plan cards only through their returned interaction endpoint.
The full request and response contract is in `docs/openapi.yaml`.

`SANDBOXD_DEFAULT_AGENT` (default `opencode`) chooses the agent when the task
does not specify one. `continue` is tri-state and defaults to continuing the
sandbox's most recent agent session. Claude Code and OpenCode use their CLI
resume behavior; one-shot GitHub Copilot tasks use a sandbox-keyed
control-plane mapping. Omit it to resume when a prior session exists and start
fresh otherwise; set `true` or `false` to force the choice. Conversation turns
always continue their active conversation until it is reset.

## Configuration reference

All settings are optional; defaults are shown below.

| Env var | Default | What it does |
| --- | --- | --- |
| `SANDBOXD_AGENT_PROXY_URL` | `http://sandboxd:9100` | In-network credential-injecting proxy URL. Empty selects the legacy mounted-auth model. |
| `SANDBOXD_DEFAULT_AGENT` | `opencode` | Agent for tasks that do not specify one. |
| `SANDBOXD_OPENCODE_ZEN_PATH` | `zen` | OpenCode Zen endpoint: `zen` (pay-as-you-go) or `zengo` (go-subscription models). |
| `SANDBOXD_OPENCODE_MODEL` | *(unset)* | Global default model for OpenCode tasks without one. |
| `SANDBOXD_MINIMAX_REGION` | `global_en` | MiniMax direct endpoint region: `global_en` or `cn_zh`. |
| `SANDBOXD_COPILOT_BRIDGE_ADDR` | `0.0.0.0:9200` | Internal listener address for the Copilot task bridge. Do not publish it. |
| `SANDBOXD_COPILOT_BRIDGE_URL` | `http://sandboxd:9200` | In-network bridge URL injected into sandboxes. sandboxd derives an IP-address URL for gVisor. |

Per task, `POST /tasks` also accepts `agent`, `model`, and `continue`.
