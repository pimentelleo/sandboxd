# Isolation & the security model

sandboxd runs AI-generated code. The security guarantee depends on the deployment
profile; do not apply the local Docker guarantee to production.

## Local Docker profile

The sandbox is one Docker container per app. The agent and generated code run
inside it, while the control plane mounts the Docker socket and is therefore
effectively host-root. Sandboxes are hardened with dropped capabilities,
no-new-privileges, a read-only root filesystem, non-root execution, and
memory/PID/file-descriptor limits. Workspaces are disposable directory mounts;
provider credentials remain control-plane-side and never enter the sandbox.

This is a shared-kernel container boundary, not a VM boundary. A kernel or
daemon escape can cross sandboxes and reach the host. The portable Compose build
also has open network egress, including LAN and cloud metadata access. Use it
for a local operator or trusted team on a host/network you control, not for
mutually distrusting tenants. Never expose its host-root control-plane API
without local auth enabled. See [production-safety.md](production-safety.md).

## Production AKS profile

The production target is multi-user within one Entra tenant and runs sandboxes
with AKS Kata Pod Sandboxing on Azure Linux/Gen2 nodes. Kubernetes, rather than
the local Docker CLI, manages sandbox pods. Azure CNI Cilium applies default
deny: a sandbox may use public HTTPS DNS egress, but Azure metadata and
internal/private network access are blocked. The console and previews are
HTTPS-only.

Production identity is OIDC authorization-code PKCE with server-side sessions.
`sandboxd.user` and `sandboxd.admin` are Entra roles; ownership is bound to the
immutable Entra OID. An owner or authorized admin receives a secure one-time
preview bootstrap ticket, exchanged for a host-only cookie. Admins have full
access. Password and API-key authentication are not production mechanisms.

Provider credentials never enter sandboxes. Hosted GitHub Copilot is a trusted
control-plane integration; its credentials and SDK state are not mounted into
workspace pods.

Kata is a stronger VM-backed boundary than ordinary containers, but it is not a
blanket guarantee. Validate Azure Linux/Gen2 nested-virtualization support for
the selected node SKU and capacity, test Azure Disk storage performance, and
verify Defender for Containers support for the chosen Kata configuration.

## Data and operations

Production workspaces use Azure Disk PVCs and Kubernetes VolumeSnapshots;
control-plane state uses PostgreSQL Flexible Server. Back up PostgreSQL and
retain tested PVC snapshots according to the recovery objective. Local
directory/SQLite data is not migrated automatically; export and import it
separately, with application-level verification.

## Reporting

Security issues: **do not open a public issue** — use
[private vulnerability reporting](https://github.com/tastyeffectco/sandboxd/security/advisories/new).
Full policy and the in-scope/out-of-scope trust model: [SECURITY.md](../SECURITY.md).
