#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
# Explicitly start an isolated test control plane. Never use the current kubeconfig.
if [ -z "${KUBEBUILDER_ASSETS:-}" ]; then
  KUBEBUILDER_ASSETS=$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2 use -p path 1.37.x)
  export KUBEBUILDER_ASSETS
fi
go test -race -count=1 -tags=integration ./internal/controller -run "TestDisposableAPIServer|TestExamplePermissionsAndLeaderFailover" -v
