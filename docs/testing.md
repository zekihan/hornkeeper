# Testing

## Local checks

`make lint test build` checks formatting, golangci-lint, race-enabled unit tests and
the application build. `make test/integration` starts a disposable Kubernetes API
server and etcd using envtest, with the upstream v1.13.0 Volume/BackupTarget schemas.
It does not use kubeconfig. No controller hooks or application CRDs are added.

The integration case exercises pending-to-bound PVC transitions, restored CSI
handles, label overrides/removal, target creation, drift correction, disabling,
real resourceVersion conflicts and unrelated fields. Its periodic interval is five
minutes and event assertions time out after fifteen seconds, so successful checks
exercise watches rather than periodic polling. A second case validates the example Deployment/Service/RBAC manifests, verifies
allowed and forbidden service-account operations, and checks that only the leader
reconciles before a standby takes over and corrects drift. Test control-plane processes stop
on cleanup. It has no kube-controller-manager; tests explicitly set binding status.

`make container-test` builds a local image and verifies its version, nonroot user,
read-only execution without capabilities, and rejection of invalid startup defaults
with networking disabled. It does not publish the image.

## Full storage validation in a disposable Linux cluster

Use a fresh Linux VM/cluster dedicated to this test. Longhorn requires privileged
storage components, host dependencies such as open-iscsi and real writable disks;
a stock macOS kind cluster is not an adequate storage-engine test. Never use the
homelab context for this exercise.

1. Create a temporary Linux Kubernetes cluster with enough nodes/disks for the
   desired replica count. Install Longhorn v1.13.0 and its documented host
   prerequisites. Set its namespace explicitly to match hornkeeper.
2. Configure two disposable S3-compatible backup targets with separate empty
   buckets/prefixes and test credentials. Use names `default` and `rustfs`. Do not
   reuse production credentials, buckets or backup targets.
3. Build/load a local hornkeeper image into that cluster, replace the example image
   reference, and apply `deploy/` against the explicit disposable kubeconfig.
4. Create one unlabelled PVC and one PVC with enabled only. Write known content.
   Check the unlabelled volume stays unchanged and the enabled one gets defaults.
5. Add target/replica overrides; observe spec changes separately from Longhorn's
   eventual replica scheduling/rebuilding. Check healthy replicas after writes.
6. Remove each override; check that field returns to default. Manually drift both
   fields and confirm correction. Disable management, drift again and confirm
   settings persist. Remove enabled entirely and confirm the same behavior.
7. Try replicas `0`, `21` and a strict-local volume with replicas `2`. Check logs and
   unchanged replica specs while a valid target update still works. Try a missing
   target and confirm a valid replica update still works. Create the missing test
   target and confirm retry succeeds.
8. Take a backup at `default`, switch to `rustfs`, wait for the spec patch and request
   another backup. Verify the recorded target and actual object locations. While a
   larger backup runs, change the target and check that the active operation finishes
   on its selected destination; a later operation goes to the new destination.
9. Restore a backup under a different volume name and bind it to an enabled PVC.
   Verify reconciliation uses the CSI handle and preserves restore source fields.
10. Run two hornkeeper instances with the same election namespace/configuration.
    Inspect the Lease, stop the leader and verify the standby takes over and corrects
    subsequent drift. Check health/readiness and metrics during failover.
11. Destroy only the disposable cluster/VM and its test buckets after recording
    results. Never run this teardown against shared storage or production contexts.

This repository's automated tests do not claim completion of those runtime checks.
