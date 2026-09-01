# Local Kind validation profile

This directory contains the PowerShell assets for exercising the Kubernetes
runtime locally. It is an integration and smoke-test profile, not a production
deployment: it uses SQLite, local email/password accounts, HTTP, a local-path
PVC, and no Kata RuntimeClass.

## Prerequisites

- PowerShell, Podman, and Kind installed on the host.
- A running Kind cluster named `kind-cluster` (or a name passed with
  `-ClusterName`) using the Podman provider.
- Contour/Envoy already installed in that cluster with its HTTP listener
  published to host port `9090`.
- A `standard` StorageClass available in the cluster.

The scripts use `kind get nodes`, `podman`, and the `kubectl` binary inside the
Kind node. They do not create a cluster or install Contour.

## Build and deploy

From the repository root:

```powershell
.\infra\dev-kind\build-and-load.ps1
.\infra\dev-kind\deploy.ps1
```

Pass `-ClusterName <name>` to either script when the cluster is not named
`kind-cluster`. The build script creates the local `:kind` images, loads them
into the Kind node, and removes its image archive. The deploy script applies the
local manifests and waits for the control plane and console rollouts.

Verify the routed readiness endpoint:

```powershell
curl.exe --noproxy "*" -H "Host: console.localhost:9090" http://127.0.0.1:9090/readyz
```

Open `http://console.localhost:9090` to bootstrap the first local account. That
account is an administrator; administrators can create normal accounts through
the console or `POST /v1/auth/accounts`. Preview URLs are emitted as
`http://s-<lowercase-id>-3000.preview.localhost:9090`; use the exact URL
returned by the API.

## Lifecycle and limits

Each sandbox receives a namespace, one `ReadWriteOnce` PVC, a single-replica
Deployment, and a private Service. Stop scales the Deployment to zero; start
scales it back to one and retains the PVC. The local permissions init container
is intentionally scoped to this profile so retained local-path volumes remain
writable after a restart.

This profile is allowed to use only:

- `SANDBOXD_PLATFORM=kubernetes-local`
- SQLite and one control-plane replica
- `PREVIEW_DOMAIN=localhost`, HTTP, and public port `9090`
- `SANDBOXD_KUBERNETES_STORAGE_CLASS=standard`
- an empty `SANDBOXD_KUBERNETES_RUNTIME_CLASS`

It does not emulate production Entra authentication, PostgreSQL, Key Vault,
HTTPS, Azure Disk, Cilium enforcement, or Kata isolation.

## Remove

```powershell
.\infra\dev-kind\remove.ps1
```

This deletes the `sandboxd-system` namespace and its local SQLite state PVC.
Sandbox namespaces are independent resources; purge their apps before removal
or delete the specific retained sandbox namespaces separately when a full
cluster cleanup is intended.
