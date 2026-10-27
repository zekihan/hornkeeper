# OPS-43 owner acceptance prerequisites

- [ ] Verify the existing Docker Hub token is limited to publishing this repository.
  Token scope is external configuration; no credentials were read or rotated here.
- [ ] Review branch/tag protection, allowed Actions, and optional protected release
  environments with an owner. This change does not create or modify live settings.
- [ ] After review and explicit publication approval, perform an intended release
  and verify GitHub assets, both image architectures in both registries, tags and
  the OCI Helm chart. Local publishers are mocked; no real publish was executed.
- [ ] Repair the existing integration-test asset bootstrap separately. Without
  `KUBEBUILDER_ASSETS`, `make test/integration` fails because the existing
  `sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2` module does not contain
  that package. This same command exists on main; it is outside OPS-43 scope.
  All three integration tests passed with the already-installed Kubernetes 1.37.0
  envtest binaries supplied through `KUBEBUILDER_ASSETS` (disposable local API server).

Local evidence: workflow policy and 13 regression tests, actionlint, Go race/integration tests,
GoReleaser configuration, Go lint/module verification, build, Helm strict lint, 11 Python chart tests, Go Helm
tests/chart packaging, disposable container smoke, a real multi-platform OCI export
(amd64/arm64), and a nonpublishing GoReleaser snapshot with checksum verification passed. No Actions retries,
billing/runner changes, production releases, or deployment were attempted.
