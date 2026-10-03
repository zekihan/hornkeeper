# hornkeeper Helm chart

Install one hornkeeper release per cluster. Longhorn v1.13.0 and the selected
backup target must already exist. Only PVCs labelled
`hornkeeper.noqer.com/enabled: "true"` are managed.

```sh
helm upgrade --install hornkeeper oci://ghcr.io/zekihan/charts/hornkeeper \
  --version 0.1.0 --namespace hornkeeper --create-namespace \
  --set controller.defaultBackupTarget=rustfs \
  --set controller.defaultReplicas=2
```

The chart and default application image are public. For a private image override,
create the registry secret in the release namespace and set `imagePullSecrets`.
The chart creates no namespaces, Longhorn resources, CRDs or hooks. The release
namespace and `controller.longhornNamespace` must exist before installation;
`--create-namespace` creates only the release namespace.

| Setting | Default | Meaning |
| --- | --- | --- |
| `replicaCount` | `2` | Controller instances; requires leader election above one |
| `image.repository` | `ghcr.io/zekihan/hornkeeper` | Application image repository |
| `image.tag` | `""` | Uses the chart's `appVersion` when empty |
| `image.digest` | `""` | Optional SHA-256 digest, taking precedence over the tag |
| `image.pullPolicy` | `IfNotPresent` | Kubernetes image pull policy |
| `imagePullSecrets` | `[]` | List of `{name: secret-name}` objects |
| `controller.longhornNamespace` | `longhorn` | Namespace watched for volumes and targets; also scopes RBAC |
| `controller.defaultBackupTarget` | `default` | Existing target for enabled PVCs without an override |
| `controller.defaultReplicas` | `3` | Desired Longhorn replicas, from 1 to 20 |
| `controller.resyncInterval` | `1m` | Positive reconciliation interval |
| `controller.leaderElection` | `true` | Lease coordination in the release namespace |
| `controller.logLevel` | `info` | `debug`, `info`, `warn` or `error` |
| `rbac.create` | `true` | Create least-privilege cluster and namespace permissions |
| `serviceAccount.create` | `true` | Create the controller ServiceAccount |
| `serviceAccount.name` | `""` | Generated name; required when using an existing account |
| `serviceAccount.annotations` | `{}` | ServiceAccount annotations |
| `metrics.enabled` | `true` | Enable the metrics listener and ClusterIP Service on port 8080 |
| `nameOverride`, `fullnameOverride` | `""` | Resource naming overrides |
| `podAnnotations` | `{}` | Pod annotations |
| `podSecurityContext`, `securityContext` | See `values.yaml` | Non-root, read-only, no capabilities, RuntimeDefault seccomp |
| `resources` | See `values.yaml` | Requests: 50m CPU/64Mi memory; memory limit: 256Mi |
| `nodeSelector`, `affinity` | `{}` | Pod scheduling constraints |
| `tolerations` | `[]` | Pod scheduling tolerations |

With `rbac.create=false`, provision the same permissions externally before
starting the controller. Disabling leader election requires `replicaCount=1`.
The health listener and probes remain enabled when metrics are disabled.

Changing defaults affects all enabled claims without the corresponding override.
Updating the desired replica count does not mean Longhorn has finished rebuilding.
See the application's README for label behavior, permissions and troubleshooting.

Release tags must match the repository's `VERSION`. The container release workflow
publishes the chart with that version and `appVersion` after the application image
has been published. Chart source lives in this repository; ArgoCD can consume its
OCI package without copying the chart templates.
