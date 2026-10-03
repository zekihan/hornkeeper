# Longhorn v1.13.0 contract

Verified against `longhorn/longhorn-manager` tag v1.13.0 at commit
`20d9c0790312b45aafd5668674e73dbf2eba1832`. The controller observes the actual
installed API through Kubernetes discovery and relies on the Longhorn admission
webhook for validation of the complete volume.

| Contract | Upstream source |
| --- | --- |
| Namespaced Volume spec: string `backupTargetName`, integer `numberOfReplicas` | [volume.go](https://github.com/longhorn/longhorn-manager/blob/20d9c0790312b45aafd5668674e73dbf2eba1832/k8s/pkg/apis/longhorn/v1beta2/volume.go) |
| Served API schemas, including backup targets | [crds.yaml](https://github.com/longhorn/longhorn-manager/blob/20d9c0790312b45aafd5668674e73dbf2eba1832/k8s/crds.yaml) |
| Replica bounds 1–20 | [setting.go](https://github.com/longhorn/longhorn-manager/blob/20d9c0790312b45aafd5668674e73dbf2eba1832/types/setting.go), `SettingDefinitionDefaultReplicaCount` |
| Strict-local requires 1 | [types.go](https://github.com/longhorn/longhorn-manager/blob/20d9c0790312b45aafd5668674e73dbf2eba1832/types/types.go), `ValidateDataLocalityAndReplicaCount` |
| Updates, target existence, sharded and clone restrictions | [validator.go](https://github.com/longhorn/longhorn-manager/blob/20d9c0790312b45aafd5668674e73dbf2eba1832/webhook/resources/volume/validator.go), `Update`, `validateReplicaCount`, `validateShardedConstraints`, `validateBackupTarget` |
| REST actions have separate operational gates | [manager/volume.go](https://github.com/longhorn/longhorn-manager/blob/20d9c0790312b45aafd5668674e73dbf2eba1832/manager/volume.go), `UpdateReplicaCount`, `UpdateVolumeBackupTarget` |
| Destination selection and backup initiation | [backup_controller.go](https://github.com/longhorn/longhorn-manager/blob/20d9c0790312b45aafd5668674e73dbf2eba1832/controller/backup_controller.go), `getBackupTargetName`, `reconcile` |

## Validation boundary

Hornkeeper validates startup defaults and label syntax, target existence and
non-deletion, the replica range, strict-local/sharded replica counts and the legacy
linked-clone replica immutability marker. It leaves the invalid field unchanged.

The target resource's existence is sufficient for Longhorn's target update
validator; it does not prove credentials, connectivity or a usable backup URL.
Hornkeeper never reads Secrets or attempts a backup to test a target.

Longhorn's webhook validates the full resulting volume. Linked clones cannot
exceed their source's replica count; reducing a source's replicas is constrained
by dependent clones and replicas. Hornkeeper does not inspect replica objects or
reimplement the dependency graph. A rejected replica patch produces a diagnostic
and retry while a valid target patch can succeed, and vice versa. This also covers
transient restrictions and changes racing with the controller's validation.

Both fields are mutable through the CR API, subject to admission. There is no
attachment prerequisite in the generic replica count CR update validation.
Longhorn's REST replica action separately checks attached state, engine upgrade
and migration; hornkeeper uses the CR API and does not invoke that action. Updating
spec is distinct from completing engine operations. It does not bypass or disable
admission webhooks. Keep the normal Longhorn webhooks enabled in production.

## Backup transition

The backup controller resolves an explicit Longhorn backup-target label first,
otherwise using the snapshot volume's `spec.backupTargetName`. Initiating a backup
passes the chosen target to the engine; changing the volume spec does not restart
that operation. Existing backups and already-selected queued operations can remain
at the old target. New operations selected after Longhorn observes the patch use
the configured target unless their own explicit target label selects another.

Reconciliation, watch delivery, Longhorn reconciliation and job timing add delay;
there is no fixed completion interval. A controller patch log confirms desired
configuration, not storage I/O. Verify the spec and a future test backup's recorded
destination when checking end-to-end behavior in a disposable cluster. No backup
migration is attempted.

## Compatibility checks

The checked-in test schemas are the exact v1.13.0 schemas with the Helm metadata
label template omitted. Envtest tests the Kubernetes API and event wiring, not
Longhorn webhook execution. Unit tests cover hornkeeper's local validation;
upstream source inspection establishes the version-specific rules. Full Longhorn
runtime validation is a separate disposable-cluster exercise.

Kubernetes libraries are pinned in `go.mod`. The local disposable API-server test
uses Kubernetes 1.37.0. Other Kubernetes server versions and other Longhorn versions
have not received full runtime verification here.
