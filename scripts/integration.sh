#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
# Explicitly start an isolated test control plane. Never use the current kubeconfig.
if [ -z "${KUBEBUILDER_ASSETS:-}" ]; then
  # setup-envtest is a nested module; pin its official v0.25.2 source commit,
  # rather than resolving the parent controller-runtime release as a package.
  # Pin both the Kubernetes release and its checksum-bearing upstream index.
  KUBEBUILDER_ASSETS=$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@5d8467b3065818a3b382d8f6941147a801fd1969 \
    use --bin-dir "${ENVTEST_BIN_DIR:-$PWD/tmp/envtest}" --verify=true \
    --index https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/1031496fc98a4f51010c3bdfdeb57b5d67bea7bd/envtest-releases.yaml \
    -p path 1.37.0)
  export KUBEBUILDER_ASSETS
fi
go test -race -count=1 -tags=integration ./internal/controller -run "TestDisposableAPIServer|TestExamplePermissionsAndLeaderFailover|TestStartupWithBundledRBAC" -v
