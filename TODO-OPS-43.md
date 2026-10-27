# OPS-43 owner acceptance prerequisites

- [ ] Verify the existing Docker Hub token is limited to publishing this repository.
  Token scope is external configuration; no credentials were read or rotated here.
- [ ] Review branch/tag protection, allowed Actions, and optional protected release
  environments with an owner. This change does not create or modify live settings.
- [ ] After review and explicit publication approval, perform an intended release
  and verify GitHub assets, both image architectures in both registries, tags and
  the OCI Helm chart. Local publishers are mocked; no real publish was executed.


Local evidence: workflow policy and 17 regression tests, actionlint, Go race/integration tests,
GoReleaser configuration, Go lint/module verification, build, Helm strict lint, 11 Python chart tests, Go Helm
tests/chart packaging, disposable container smoke, a real multi-platform OCI export
(amd64/arm64), and a nonpublishing GoReleaser snapshot with checksum verification passed. No Actions retries,
billing/runner changes, production releases, or deployment were attempted.

Integration bootstrap is repaired: the exact official setup-envtest commit,
Kubernetes 1.37.0, and checksum-bearing release index are pinned. All three
integration tests passed with `KUBEBUILDER_ASSETS` unset and a newly created empty
`ENVTEST_BIN_DIR`, downloading and verifying fresh binaries before testing.
