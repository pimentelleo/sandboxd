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

podman exec $node kubectl delete namespace sandboxd-system --ignore-not-found --wait=true
if ($LASTEXITCODE -ne 0) {
  throw "Unable to delete the sandboxd-system namespace."
}
