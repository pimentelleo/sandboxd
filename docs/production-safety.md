# Production safety

Production is a separate, multi-user profile for one Entra tenant. The default
Compose install is local Docker/SQLite and is not a production security
baseline. Read [`isolation.md`](isolation.md) before choosing a profile.

## Local profile checklist

Use this for a single operator or trusted team on a host you control:

- [ ] Keep the API on loopback or enable `SANDBOXD_API_AUTH_DISABLED=false`.
- [ ] If the API is authenticated, configure local `SANDBOXD_API_TOKENS`;
      password-backed console sessions remain supported.
- [ ] Use a real HTTPS reverse proxy before exposing previews.
- [ ] Patch the host kernel and Docker Engine.
- [ ] Back up workspaces and SQLite state under `SANDBOXD_DATA_DIR`.
- [ ] Treat egress as open by default; do not keep unrelated secrets on the host.

These controls do not turn shared-kernel Docker into hostile multi-tenancy.

## Production AKS checklist

The production deployment artifacts and their exact parameters are documented
in [`../infra/README.md`](../infra/README.md). They are not applied
automatically, and cloning this repository does not provision Azure resources.
Before a gated deployment:

- [ ] Use an AKS cluster with Kata Pod Sandboxing on supported Azure Linux/Gen2
      nodes; validate nested virtualization, SKU capacity, and quota.
- [ ] Use PostgreSQL Flexible Server for control-plane state and Azure Disk PVCs
      for workspaces. Configure backups, restore drills, and retained
      Kubernetes VolumeSnapshots.
- [ ] Use Key Vault with workload identity for secrets and ACR for images.
- [ ] Use Azure CNI Cilium policies with default deny. Allow only required
      control-plane traffic and public HTTPS DNS egress from sandboxes; block
      Azure metadata and internal/private network access.
- [ ] Publish only HTTPS ingress: `console.<domain>` and
      `*.preview.<domain>`, with certificates and DNS under operator control.
- [ ] Configure one Entra tenant and assign only `sandboxd.user` or
      `sandboxd.admin`. Ownership must use the immutable Entra OID; admins have
      full access.
- [ ] Verify OIDC authorization-code PKCE and server-side session behavior.
      Production has no password or API-key login.
- [ ] Verify preview authorization: owner/admin access uses a secure one-time
      bootstrap ticket and host-only cookies. Preview requests must arrive through
      the TLS gateway; the gateway rejects client-supplied forwarded-proto headers,
      removes tickets from the URL, and never forwards identities, credentials, or
      gateway cookies to the sandbox. Do not put secrets in previews.
- [ ] Confirm provider credentials never enter sandbox pods. Hosted GitHub
      Copilot remains trusted control-plane infrastructure. The only permitted
      sandbox-to-control-plane flow is the port 9100 credential-injecting agent proxy.
- [ ] Test Kata workload behavior, Azure Disk performance, and Defender for
      Containers support for the selected configuration.

There is no automatic migration from local directory/SQLite state to
PostgreSQL/PVCs. Treat any export/import as a separate, reviewed data migration.

## Trust boundaries

The control plane is privileged orchestration infrastructure. In local Docker it
can control the host Docker daemon. In production, Kubernetes RBAC and network
policy constrain the control plane, while Kata provides a VM-backed workload
boundary. Neither profile removes the need to patch nodes, restrict ingress,
review images, monitor audit logs, and test recovery.
