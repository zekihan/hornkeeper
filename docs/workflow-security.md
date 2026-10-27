# Workflow security (OPS-43)

All external workflow Actions are full commit pins with release-version comments.
The table records the existing release tags resolved through the official upstream
GitHub API (annotated tags peeled to commits) on 2026-10-27. Pinned action manifests
were reviewed for the inputs/runtime used here; these pins do not assert an audit
of every transitive dependency. Renovate proposes pinned updates without automerge;
reviewers must recheck the tag-to-commit mapping and source diff before merging.

| Upstream release | Commit |
| --- | --- |
| `actions/checkout@v7.0.1` | [`3d3c42e5aac5ba805825da76410c181273ba90b1`](https://github.com/actions/checkout/commit/3d3c42e5aac5ba805825da76410c181273ba90b1) |
| `actions/download-artifact@v8.0.2` | [`9000827ccba6bdab643e8b6fd33ac0654aef8333`](https://github.com/actions/download-artifact/commit/9000827ccba6bdab643e8b6fd33ac0654aef8333) |
| `actions/setup-go@v7.0.0` | [`b7ad1dad31e06c5925ef5d2fc7ad053ef454303e`](https://github.com/actions/setup-go/commit/b7ad1dad31e06c5925ef5d2fc7ad053ef454303e) |
| `actions/upload-artifact@v7.0.2` | [`cf430e030ddbb5b0abf93d22962f4752f3646cd9`](https://github.com/actions/upload-artifact/commit/cf430e030ddbb5b0abf93d22962f4752f3646cd9) |
| `azure/setup-helm@v5.0.1` | [`9bc31f4ebc9c6b171d7bfbaa5d006ae7abdb4310`](https://github.com/azure/setup-helm/commit/9bc31f4ebc9c6b171d7bfbaa5d006ae7abdb4310) |
| `docker/build-push-action@v7.4.0` | [`c3c9e263c25d99ce0380d002d59b67737d91b0dc`](https://github.com/docker/build-push-action/commit/c3c9e263c25d99ce0380d002d59b67737d91b0dc) |
| `docker/metadata-action@v6.2.0` | [`dc802804100637a589fabce1cb79ff13a1411302`](https://github.com/docker/metadata-action/commit/dc802804100637a589fabce1cb79ff13a1411302) |
| `docker/setup-buildx-action@v4.4.1` | [`f87e5991a6d7451dcb8d9637bfbc97413f497069`](https://github.com/docker/setup-buildx-action/commit/f87e5991a6d7451dcb8d9637bfbc97413f497069) |
| `docker/setup-qemu-action@v4.4.0` | [`99012661954931238ded8c8b007157a8430204e1`](https://github.com/docker/setup-qemu-action/commit/99012661954931238ded8c8b007157a8430204e1) |
| `golangci/golangci-lint-action@v9.3.0` | [`ba0d7d2ec06a0ea1cb5fa41b2e4a3ab91d21278a`](https://github.com/golangci/golangci-lint-action/commit/ba0d7d2ec06a0ea1cb5fa41b2e4a3ab91d21278a) |
| `goreleaser/goreleaser-action@v7.3.0` | [`a4f02ab5355a589de60d917a659ce617ed19b3de`](https://github.com/goreleaser/goreleaser-action/commit/a4f02ab5355a589de60d917a659ce617ed19b3de) |

## Release boundary

Reusable read-only checks gate read-only build jobs. GoReleaser produces archives
and checksums without publishing. The container build exports a multi-platform OCI
archive and packages the chart, then hashes both. Upload artifacts are immutable;
publishers download the exact artifact ID returned by that build in the same run,
not a mutable artifact name. Checksums detect corrupted content before credentials
are introduced. Trust remains in reviewed repository source, pinned Actions, and
GitHub's runner/artifact service; checksums are not an independent signature.

Fresh publishers never check out repository source. Only the GitHub release job
has contents:write; only the image/chart publisher has packages:write. Registry
credentials are step scoped and temporary authentication files are removed on exit.
Skopeo copies every platform from the verified OCI archive while preserving digests;
Helm pushes the already-packaged chart. GitHub CLI publishes the verified archives
with generated release notes. GoReleaser still builds the original archive contents.
Release and manual container triggers remain unchanged.

## Local validation

```sh
python3 -m venv /tmp/hornkeeper-workflows
/tmp/hornkeeper-workflows/bin/pip install -r tests/helm/requirements.txt
/tmp/hornkeeper-workflows/bin/python scripts/check_workflow_policy.py
/tmp/hornkeeper-workflows/bin/python -m unittest discover -s tests/workflows -v
actionlint
```

The checks workflow runs this policy and its regression tests before publication.
Tests exercise mutable reference rejection, scope separation, persisted checkout
credentials, artifact-ID selection, checksum tampering, and the actual publisher
shell commands with mocked registry/Helm/GitHub clients. No real publication occurs.
The shell mock requires Bash 4+ (as supplied by Ubuntu runners); on macOS the test
uses Homebrew Bash when available.

## Reproducible integration bootstrap

`make test/integration` runs the complete disposable Kubernetes gate without
requiring preinstalled assets. `scripts/integration.sh` pins setup-envtest's
nested module to the official controller-runtime v0.25.2 commit
[`5d8467b3065818a3b382d8f6941147a801fd1969`](https://github.com/kubernetes-sigs/controller-runtime/tree/5d8467b3065818a3b382d8f6941147a801fd1969/tools/setup-envtest).
The parent module's version previously failed package resolution; the explicit
source commit resolves the executable's nested module. Kubernetes 1.37.0 is
selected exactly, with checksum verification enabled against the official
[release index pinned at 1031496fc98a4f51010c3bdfdeb57b5d67bea7bd](https://github.com/kubernetes-sigs/controller-tools/blob/1031496fc98a4f51010c3bdfdeb57b5d67bea7bd/envtest-releases.yaml).
Review both sources and checksums when updating the bootstrap.

Assets default to `tmp/envtest` in this checkout. An explicit
`KUBEBUILDER_ASSETS` still permits locally managed binaries. To verify a fresh
download independently of an existing installation:

```sh
env -u KUBEBUILDER_ASSETS ENVTEST_BIN_DIR="$(mktemp -d)" make test/integration
```

The fresh-download path passed all three integration tests. Regression fixtures
also ensure bootstrap failure prevents test execution, test failures propagate,
and the supplied-assets path retains the entire race-enabled integration gate.
