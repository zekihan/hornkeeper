# hornkeeper

A small Go Kubernetes controller that keeps opted-in Longhorn volumes' backup
target and desired replica count in sync with PVC labels. It manages existing and
restored volumes as well as newly bound claims. It uses Longhorn's existing API;
there are no hornkeeper CRDs, deployment hooks or Helm chart.

## Labels

```yaml
metadata:
  labels:
    hornkeeper.noqer.com/enabled: "true"
    hornkeeper.noqer.com/backup-target: rustfs
    hornkeeper.noqer.com/replicas: "2"
```

Only the exact string `"true"` enables management. Missing enabled labels,
`"false"`, `"TRUE"` and every other value leave the volume untouched, even when
there are override labels. PVCs in every namespace are observed; only bound
`driver.longhorn.io` PVs qualify.

| PVC labels | Effective behavior |
| --- | --- |
| enabled only | Enforce controller defaults for both fields |
| enabled plus backup-target | Override only the backup target |
| enabled plus replicas | Override only the desired replica count |
| enabled plus both overrides | Enforce both overrides |
| remove an override while enabled | Return that field to its controller default |
| disable or remove enabled | Stop enforcement; keep current volume settings |
| invalid override while enabled | Keep the affected field; reconcile the other independently |

An explicitly empty override is invalid. It does not mean "use the default".
Backup target names must refer to an existing, non-deleting BackupTarget in the
configured Longhorn namespace. Replicas must be decimal integers from 1 to 20;
strict-local and sharded volumes require 1. Legacy linked-clone volumes cannot
change replica count. Longhorn admission validates further clone dependencies.

The controller writes only `Volume.spec.backupTargetName` and
`Volume.spec.numberOfReplicas`. It does not create targets, migrate backups,
modify recurring jobs, replicas, credentials or backup resources. In the homelab,
`default` is the existing Backblaze target and `rustfs` is the existing RustFS
target. Their configuration remains owned by Longhorn and your deployment.

## Build and run

Use the Go version already specified in `go.mod`:

```sh
make build
./dist/hornkeeper --help
./dist/hornkeeper --version
./dist/hornkeeper --longhorn-namespace=longhorn \
  --default-backup-target=default --default-replicas=3 \
  --leader-election-namespace=hornkeeper
```

The binary uses in-cluster credentials in Kubernetes, or the standard kubeconfig
when run locally. A normal run is an active controller and can modify opted-in
volumes in that cluster. Use the disposable integration tests for development.

Configuration is supplied through command-line flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--longhorn-namespace` | `longhorn` | Namespace for Volume and BackupTarget resources |
| `--default-backup-target` | `default` | Target for enabled PVCs without a target override |
| `--default-replicas` | `3` | Replica count for enabled PVCs without a replica override |
| `--resync-interval` | `1m` | Periodic retries for enabled claims, including invalid or pending claims |
| `--leader-elect` | `true` | Elect a single active controller using a Lease |
| `--leader-election-namespace` | `hornkeeper` | Namespace for Lease `hornkeeper.noqer.com` |
| `--metrics-bind-address` | `:8080` | HTTP metrics address; `0` disables it |
| `--health-probe-bind-address` | `:8081` | HTTP health and readiness address |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error` |
| `--version` | — | Print version and exit without Kubernetes access |

Namespaces, default target syntax, replica range and positive retry interval are
validated before starting. The default target must exist at startup; its absence,
deletion, unavailable API or denied permissions causes startup to fail with a
structured diagnostic. Changing deployment defaults changes every enabled PVC
that has no corresponding override. Defaults do not inherit StorageClass or
Longhorn global settings.

## Kubernetes example

[deploy/](deploy/) contains plain manifests and a Kustomize entrypoint; the image
reference is an example and must be replaced with an image you have built and
published. No image has been published as part of this repository setup.

Before applying the example, set the image, controller defaults and namespace
arguments. If Longhorn uses another namespace, change both the argument and the
Longhorn Role/RoleBinding namespaces in `deploy/rbac.yaml`. Keep the Lease
namespace argument aligned with its Role/RoleBinding. Longhorn and backup targets
must already exist. Configure `imagePullSecrets` when using a private GHCR image.

```sh
# Run only against the cluster you intend to manage.
kubectl apply -k deploy
kubectl apply -f examples/pvc.yaml
```

The example has two instances with leader election enabled. Each instance watches
resources and has a synchronized cache; only the elected leader reconciles.
Use the same Longhorn namespace, defaults and Lease namespace for every instance.
Different policies must not run against the same PVCs. With leader election
disabled, run exactly one instance. Lease failover takes up to the default
15-second lease duration, plus scheduling/API delays. SIGTERM stops work and
allows up to 20 seconds for shutdown; the pod grace period is 30 seconds.

The image runs as UID/GID 25000 in a scratch container with a CA bundle. The
example uses a read-only root filesystem, no Linux capabilities, seccomp,
resource limits and health/readiness probes.

### Permissions

- Cluster-wide `get/list/watch` on PVCs and PVs for binding and event mapping.
- In the Longhorn namespace, `get/list/watch/patch` on volumes and
  `get/list/watch` on backup targets.
- In the controller namespace, `create` on leases, `get/update` on the named
  election lease, and `create/patch` on events for leader election diagnostics.

No PVC/PV writes, Longhorn status writes, Secret access, or writes to targets,
replicas and backups are needed. Kubernetes RBAC cannot restrict patch access by
PVC opt-in labels or by JSON field. The service account can patch volumes in the
Longhorn namespace; the controller enforces the label and field boundaries.
Lease creation cannot be restricted by resource name in RBAC, so it is limited to
the dedicated controller namespace.

## Reconciliation and observability

An enabled claim waits until its PVC and PV are Bound and the PV claim reference
matches the current PVC namespace, name and UID. The controller resolves the
Longhorn volume from the PV CSI `volumeHandle`, never from the PVC UID. Missing
PVs/volumes are retried; existing resources are reconciled on startup. Restore
source fields are left unchanged.

PVC label/binding changes, PV changes, Longhorn volume spec changes and target
changes trigger reconciliation. Indexed reverse lookups map volume events to
claims. A periodic retry also covers missed events and transient restrictions.
Each field gets a separate resourceVersion-protected merge patch. Conflicts cause
fresh reads and bounded immediate retries, followed by the work queue's backoff.
Other failures are retried with backoff. Each reconciliation has a 30-second
context deadline; watch streams remain long-lived. Every attempt reads current opt-in and
binding directly from the API; disabling stops queued/retried work. Kubernetes
cannot atomically transact a PVC label and a Volume patch, so a patch already
submitted during a simultaneous disable can finish.

Successful patching means the desired Longhorn spec changed. **It does not mean
replica rebuilding or removal has completed.** Longhorn owns scheduling, rebuilds,
replica removal and backup execution. Increasing replicas may remain degraded if
storage, placement or availability prevents rebuilding. Inspect Longhorn health
and replica state to determine completion.

Logs are JSON on stderr. Changed fields include claim/reconcile context, volume,
field, previous and desired values. Invalid labels/constraints and rejected
patches produce field-specific diagnostics. The controller does not annotate PVCs
or emit PVC events.

`/healthz` checks the running process. `/readyz` becomes successful after all
required watch caches synchronize, including on a standby instance. Readiness
does not assert that every volume is healthy or every policy is valid.

`/metrics` exposes Go/process and controller-runtime work queue/reconciliation
metrics, plus:

- `hornkeeper_reconciliations_total{result="ignored|enabled|error"}`.
- `hornkeeper_field_reconciliations_total{field="backupTargetName|numberOfReplicas",result="patched|unchanged|invalid|waiting|error"}`.

Metrics have no PVC/volume labels, avoiding per-volume cardinality. Invalid
labels are nonfatal and show up in field metrics and logs; alerts should include
invalid fields as well as reconciliation errors. The example metrics Service is
ClusterIP. Metrics/probe HTTP has no authentication; restrict network access to
monitoring and the kubelet when your cluster policy requires it.

## Backup behavior and compatibility

Changing backup target affects future backups after the volume spec patch and
Longhorn's asynchronous observation. There is no guaranteed wall-clock delay.
Wait for the desired `spec.backupTargetName` before requesting a new backup, and
check the resulting backup's recorded target. Already running backups continue
using the target chosen for their operation; they are not restarted or migrated.
Already-created or queued backups can have a destination selected before the
change. A backup's explicit Longhorn target label takes precedence over the
volume spec. Old backups remain at their original target.

Recurring-job labels choose jobs/schedules; they do not select the destination.
StorageClass parameters set values at volume creation and do not enforce later
changes on an existing volume.

The supported contract is Longhorn **v1.13.0**, `longhorn.io/v1beta2`, with the
normal Longhorn admission webhook enabled. This controller uses Kubernetes'
unstructured client for those two existing CRDs, keeping the dependency focused
on Kubernetes libraries. It patches the CR API rather than Longhorn's REST
`updateReplicaCount` action. The REST action has attachment/upgrade/migration
gates that differ from CR admission. This controller does not require attachment
before changing desired count; rebuilding may wait until attachment or Longhorn's
offline rebuilding policy permits it. Longhorn admission remains authoritative,
including linked-clone dependency restrictions. Rejected fields remain unchanged
and are retried independently.

See [docs/compatibility.md](docs/compatibility.md) for the exact upstream API
sources and validation boundaries. Other Longhorn versions have not been
verified; review admission and controller behavior before changing versions.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Startup fails | Default target exists in the configured namespace; RBAC/discovery and API connectivity work |
| Enabled claim stays waiting | PVC/PV Bound phases, matching claimRef UID, CSI driver/handle and Longhorn volume existence |
| One field does not change | Invalid/empty override, missing target, strict-local/sharded/legacy constraints, admission rejection logs |
| Target later becomes available | Target event and periodic retry will reconcile automatically |
| Desired replicas updated but degraded | Longhorn scheduling/rebuild status, disk capacity, replica placement and engine state |
| Manual changes revert | Expected while opted in; disable management before manually owning those fields |
| No active instance | Election Lease, its namespace/permissions and consistent settings across instances |
| Readiness stays false | PVC/PV and Longhorn watch/list permissions, installed CRDs and API connectivity |
| Backup uses old target | Backup creation/selection timing, explicit target label and recorded backup destination |

## Validation and releases

```sh
make fmt lint test build
make test/integration
make container-test
make release-check
```

Unit tests cover label lifecycle, defaults/overrides, invalid fields, pending
binding, restored handles, drift, no-op writes, conflicts, admission rejection and
unrelated field preservation. `test/integration` launches a disposable etcd/API
server and installs only upstream Longhorn schemas in that test environment.
It exercises real watches, schema validation, optimistic conflicts, example RBAC
and leader failover with an impersonated service account. It never
reads the current kubeconfig or contacts the homelab. It downloads envtest binaries
on first use; set `KUBEBUILDER_ASSETS` to use an existing installation.

The integration check does **not** run Longhorn managers, admission webhooks,
engines or backup storage. Actual rebuild completion, target connectivity and
in-progress backup behavior require a separate disposable Linux cluster with
Longhorn and test backup targets. See [docs/testing.md](docs/testing.md).

GitHub Actions checks formatting/modules, lint, race tests, API-server integration,
builds, release configuration and container startup. A `v*` tag runs GoReleaser
for Linux/macOS amd64/arm64 archives and a separate GHCR container workflow for
Linux amd64/arm64. GHCR visibility is managed separately from repository visibility;
keep the package private. There is no Docker Hub README synchronization for this
private repository. Publishing and deployment are separate actions.

## License

AGPL-3.0, following the sibling service repositories. Bundled Longhorn test schemas
retain their upstream Apache-2.0 license in `internal/controller/testdata`.
