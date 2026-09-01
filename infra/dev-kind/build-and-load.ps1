param(
  [string]$ClusterName = "kind-cluster"
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$repoRoot = Split-Path -Parent $repoRoot
$archive = Join-Path $env:TEMP "sandboxd-kind-images.tar"
$baseImage = "localhost/sandboxd-base:kind"
$controlPlaneImage = "localhost/sandboxd-control-plane:kind"
$consoleImage = "localhost/sandboxd-console:kind"

function Assert-LastNativeCommandSucceeded {
  if ($LASTEXITCODE -ne 0) {
    throw "Native command failed with exit code $LASTEXITCODE."
  }
}

try {
  Push-Location $repoRoot
  podman build --tag $baseImage --file image/Dockerfile .
  Assert-LastNativeCommandSucceeded
  podman build --tag $controlPlaneImage --file control-plane/Dockerfile.production control-plane
  Assert-LastNativeCommandSucceeded
  podman build --tag $consoleImage --file console/Dockerfile.production console
  Assert-LastNativeCommandSucceeded
  podman save --format docker-archive --output $archive $baseImage $controlPlaneImage $consoleImage
  Assert-LastNativeCommandSucceeded
  kind load image-archive --name $ClusterName $archive
  Assert-LastNativeCommandSucceeded
} finally {
  Pop-Location
  if (Test-Path $archive) {
    Remove-Item $archive -Force
  }
}
