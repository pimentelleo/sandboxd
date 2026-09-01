param(
  [string]$ClusterName = "kind-cluster"
)

$ErrorActionPreference = "Stop"
$node = (kind get nodes --name $ClusterName | Select-Object -First 1)
if ($LASTEXITCODE -ne 0) {
  throw "Unable to resolve nodes for Kind cluster '$ClusterName'."
}
$node = $node.Trim()
if (!$node) {
  throw "Kind cluster '$ClusterName' has no nodes."
}

function Assert-LastNativeCommandSucceeded {
  if ($LASTEXITCODE -ne 0) {
    throw "Native command failed with exit code $LASTEXITCODE."
  }
}

$source = $PSScriptRoot.TrimEnd('\', '/')
$target = "/tmp/sandboxd-dev-kind"
podman exec $node rm -rf $target
Assert-LastNativeCommandSucceeded
podman cp $source "${node}:$target"
Assert-LastNativeCommandSucceeded
podman exec $node kubectl apply -k $target
Assert-LastNativeCommandSucceeded
podman exec $node kubectl rollout restart deployment/sandboxd-control-plane -n sandboxd-system
Assert-LastNativeCommandSucceeded
podman exec $node kubectl rollout restart deployment/sandboxd-console -n sandboxd-system
Assert-LastNativeCommandSucceeded
podman exec $node kubectl rollout status deployment/sandboxd-control-plane -n sandboxd-system --timeout=180s
Assert-LastNativeCommandSucceeded
podman exec $node kubectl rollout status deployment/sandboxd-console -n sandboxd-system --timeout=180s
Assert-LastNativeCommandSucceeded
