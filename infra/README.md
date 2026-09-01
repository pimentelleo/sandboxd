# sandboxd production infrastructure

These artifacts define a **new**, parameterized Brazil South production topology. They
do not provision anything on their own, and they do not change the local Docker/SQLite
deployment.

## Local Kind validation

[`dev-kind/`](dev-kind/README.md) is a separate, local-only integration profile
for a preconfigured Kind cluster running through Podman. It uses SQLite, local
accounts, HTTP, the `standard` StorageClass, and no Kata RuntimeClass. It is not
part of this production topology and must not be used to validate or weaken the
Entra, PostgreSQL, Key Vault, HTTPS, Cilium, or Kata requirements below.

## Required operator inputs

Create protected, non-repository parameter files from
`parameters/production.example.bicepparam` and
`parameters/tenant.example.bicepparam`. Supply a globally unique prefix; non-overlapping
VNet, AKS, private-endpoint, and delegated PostgreSQL CIDRs; an existing public Azure DNS
zone resource ID; Entra group object IDs; validated AKS/Kata VM SKU and Kubernetes
version; the AKS cluster DNS domain (normally `cluster.local`); distinct PostgreSQL
availability zones; and a PostgreSQL administrator login.
Pass the PostgreSQL password only as a secure deployment parameter; the example reads
it from `POSTGRES_ADMIN_PASSWORD` and contains no value. The pipeline also expects
protected variables `DEPLOYMENT_PRINCIPAL_OBJECT_ID`, `POSTGRES_ADMIN_PASSWORD`,
`SANDBOXD_DATABASE_URL`, `SANDBOXD_ENCRYPTION_KEY`, and
`SANDBOXD_ENTRA_CLIENT_SECRET`; it never prints them. The Entra tenant/client ID,
HTTPS callback URL, and control-plane non-root UID/GID are required pipeline parameters.

The tenant template consumes both group object IDs; group memberships themselves remain
operator-managed.

## Deployment ordering

1. Validate quota, SKU and AKS Kata support in Brazil South. Create the two existing
   Entra security groups (users and administrators) in one tenant.
2. Bootstrap the deployment identity with resource-group Contributor and User Access
   Administrator (including the existing DNS zone scope), and give its private-network
   agent connectivity to the AKS private API endpoint. `main.bicep` then assigns the
   protected deployment principal only AcrPush on the registry, Key Vault Secrets Officer
   on the vault, AKS Cluster User and AKS RBAC Cluster Admin on the cluster, and DNS Zone
   Contributor on the existing zone. These role assignments need a privileged bootstrap
   identity because a principal cannot reliably grant itself permissions mid-deployment.
3. Deploy `tenant/entra-app.bicep` at tenant scope using a principal with Microsoft Graph
   application permissions sufficient to create an application/service principal and
   assign app roles to groups (normally `Application.ReadWrite.All` and
   `AppRoleAssignment.ReadWrite.All`, admin-consented). This template uses the Bicep
   Microsoft Graph extension configured in `bicepconfig.json`; keep it separate from
   resource-group deployment.
4. Run resource-group `main.bicep` with the secure PostgreSQL parameter. It builds a
   private VNet, ACR, Key Vault, PostgreSQL Flexible Server and database, AKS, identities,
   private endpoints/DNS, diagnostics, and RBAC. It also creates `console` and
   `*.preview` A records in the parameterized existing DNS zone for the static ingress IP.
   Populate the three runtime Key Vault secrets from the protected release variables.
5. From an agent on the private network, install the pinned Helm dependencies in their
   own `cert-manager` and `traefik` namespaces, apply the foundational manifests
   server-side, wait for the migration Job, then deploy the control-plane and console
   workloads. Point the existing public DNS zone's `console.<root-domain>` and
   `*.preview.<root-domain>` A records at the emitted static ingress IP.

## Security and platform constraints

* AKS has a private management API, Azure CNI Overlay powered by Cilium, workload
  identity, Azure Disk CSI, and a multi-zone system pool. The dedicated sandbox pool is
  tainted, Azure Linux, and requests AKS Kata MSHV VM isolation. Confirm the selected
  VM SKU supports nested virtualization and that AKS publishes
  `kata-mshv-vm-isolation` in the selected version before deployment.
* The `kata-vm-isolation` RuntimeClass is an intentional alias of that AKS handler;
  sandbox Pod templates use the alias. Do not apply it until the Kata prerequisite is
  verified, or sandbox scheduling will fail.
* ACR and Key Vault have public access disabled and private endpoints. Key Vault uses
  RBAC with purge protection. PostgreSQL is VNet-injected with zone-redundant HA and no
  public endpoint.
* `sandboxd-system` enforces restricted Pod Security. The Kubernetes runtime creates one
  labeled namespace per sandbox; the cluster-wide Cilium policy default-denies each
  sandbox, admits preview traffic only from the control plane, permits DNS plus an
  explicit public-HTTPS registry/source allow-list, and blocks private, cluster,
  link-local/metadata, and Azure management destinations. The sole internal egress exception is port 9100 to the control-plane
  credential-injecting agent proxy; it forwards only to its fixed public provider list
  and prevents agent credentials from entering a sandbox. Dynamic sandbox Pods have no service-account token, Docker socket,
  privileged mode, or provider-secret mount. Provider credentials must remain in Key
  Vault/control-plane processes; never add them to a sandbox template.
* The Traefik Service explicitly declares the resource group containing the static public
  IP, and the AKS control-plane identity receives Network Contributor on that IP only.
  This allows the AKS-managed load balancer to attach an IP outside its node resource
  group without granting broad network permissions.
* The control-plane ConfigMap uses the runtime's exact Entra names:
  `SANDBOXD_AUTH_PROFILE`, `SANDBOXD_ENTRA_TENANT_ID`,
  `SANDBOXD_ENTRA_CLIENT_ID`, `SANDBOXD_ENTRA_CLIENT_SECRET`, and
  `SANDBOXD_ENTRA_REDIRECT_URL`. The client secret is provided only through Key Vault
  CSI. Cilium permits the control plane's TLS egress to `login.microsoftonline.com`;
  console DNS and control-plane DNS egress are limited to kube-dns.
* `SANDBOXD_PLATFORM=kubernetes` is required to enable the production runtime. In that
  profile, `DATABASE_URL` must be a PostgreSQL connection string; its presence alone
  never changes the Docker/SQLite local profile. The manifests set the platform
  explicitly and mount `DATABASE_URL` from Key Vault. Do not add speculative
  `SANDBOXD_DB_*` production variables.
* Data and log mounts are explicit writable `emptyDir` locations under the documented
  paths, with UID/GID supplied at release time. The production image already runs
  non-root and durable control-plane state is stored in PostgreSQL; retain no user data
  in these ephemeral locations, and use the chosen log sink for retained logs.
* The deployment starts with no sandbox Pods or user workspaces. The Go control plane's
  Kubernetes runtime creates a private Service per sandbox. A single TLS wildcard
  ingress sends all preview traffic to the control-plane gateway, which authenticates
  and authorizes it before waking or proxying to that Service; no sandbox has an
  externally routable ingress.

The migration Job invokes the PostgreSQL-only `sandboxd migrate` command before the
control plane rolls out. It fails closed if `DATABASE_URL` is absent or is not a
PostgreSQL connection string.
