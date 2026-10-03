import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "charts" / "hornkeeper"
HELM = os.environ.get("HELM", "helm")


def render(values=None, namespace="hornkeeper", release="hornkeeper"):
    with tempfile.TemporaryDirectory() as directory:
        path = Path(directory) / "values.yaml"
        path.write_text(yaml.safe_dump(values or {}))
        result = subprocess.run(
            [HELM, "template", release, str(CHART), "--namespace", namespace,
             "--values", str(path)],
            capture_output=True, text=True,
        )
    return result


def resources(values=None, namespace="hornkeeper", release="hornkeeper"):
    result = render(values, namespace, release)
    if result.returncode:
        raise AssertionError(result.stderr)
    return [item for item in yaml.safe_load_all(result.stdout) if item]


def get(items, kind, namespace=None):
    return next(item for item in items if item["kind"] == kind
                and (namespace is None or item["metadata"].get("namespace") == namespace))


class ChartTests(unittest.TestCase):
    def test_chart_versions_match_application_release(self):
        chart = yaml.safe_load((CHART / "Chart.yaml").read_text())
        version = (ROOT / "VERSION").read_text().strip()
        self.assertEqual(chart["version"], version)
        self.assertEqual(chart["appVersion"], version)

    def test_example_behavior_and_least_privilege(self):
        items = resources()
        example = yaml.safe_load((ROOT / "deploy/deployment.yaml").read_text())
        pod = get(items, "Deployment")["spec"]["template"]["spec"]
        old_pod = example["spec"]["template"]["spec"]
        container = dict(pod["containers"][0])
        container.pop("imagePullPolicy")
        self.assertEqual(container, old_pod["containers"][0])
        for key in ("serviceAccountName", "terminationGracePeriodSeconds", "securityContext"):
            self.assertEqual(pod[key], old_pod[key])
        old_roles = [item for item in yaml.safe_load_all((ROOT / "deploy/rbac.yaml").read_text())
                     if item["kind"] in ("Role", "ClusterRole")]
        roles = [item for item in items if item["kind"] in ("Role", "ClusterRole")]
        self.assertEqual([item["rules"] for item in roles], [item["rules"] for item in old_roles])
        account = get(items, "ServiceAccount")["metadata"]
        for binding in (item for item in items if item["kind"].endswith("RoleBinding")):
            self.assertEqual(binding["subjects"], [{"kind": "ServiceAccount",
                             "name": account["name"], "namespace": account["namespace"]}])
            ref = binding["roleRef"]
            self.assertTrue(any(item["kind"] == ref["kind"]
                                and item["metadata"]["name"] == ref["name"]
                                and item["metadata"].get("namespace") == binding["metadata"].get("namespace")
                                for item in roles))
        deployment = get(items, "Deployment")
        self.assertEqual(deployment["spec"]["selector"]["matchLabels"],
                         deployment["spec"]["template"]["metadata"]["labels"])
        self.assertEqual(get(items, "Service")["spec"]["selector"],
                         deployment["spec"]["selector"]["matchLabels"])
        self.assertFalse(any(item["kind"] in ("Namespace", "CustomResourceDefinition", "BackupTarget")
                             or "helm.sh/hook" in item["metadata"].get("annotations", {}) for item in items))

    def test_custom_namespaces_and_defaults(self):
        items = resources({"controller": {"longhornNamespace": "storage", "defaultBackupTarget": "rustfs",
                                          "defaultReplicas": 2, "resyncInterval": "30s", "logLevel": "debug"}},
                          namespace="operators", release="volumes")
        deployment = get(items, "Deployment")
        args = deployment["spec"]["template"]["spec"]["containers"][0]["args"]
        for arg in ("--longhorn-namespace=storage", "--default-backup-target=rustfs",
                    "--default-replicas=2", "--leader-election-namespace=operators",
                    "--resync-interval=30s", "--log-level=debug"):
            self.assertIn(arg, args)
        self.assertEqual(get(items, "Role", "storage")["metadata"]["namespace"], "storage")
        for binding in (item for item in items if item["kind"].endswith("RoleBinding")):
            self.assertEqual(binding["subjects"][0]["namespace"], "operators")
        other_namespace = resources(namespace="elsewhere", release="volumes")
        self.assertNotEqual(get(items, "ClusterRole")["metadata"]["name"],
                            get(other_namespace, "ClusterRole")["metadata"]["name"])
        self.assertIn("--leader-elect=true", args)

    def test_single_replica_without_election_or_metrics(self):
        items = resources({"replicaCount": 1, "controller": {"leaderElection": False},
                           "metrics": {"enabled": False}})
        self.assertFalse(any(item["kind"] == "Service" for item in items))
        self.assertFalse(any(item["metadata"]["name"].endswith("-leader") for item in items))
        container = get(items, "Deployment")["spec"]["template"]["spec"]["containers"][0]
        self.assertIn("--leader-elect=false", container["args"])
        self.assertIn("--metrics-bind-address=0", container["args"])
        self.assertEqual(container["ports"], [{"name": "health", "containerPort": 8081}])
        self.assertEqual(container["readinessProbe"]["httpGet"]["port"], "health")

    def test_existing_account_image_digest_and_scheduling(self):
        digest = "sha256:" + "a" * 64
        items = resources({"rbac": {"create": False},
                           "serviceAccount": {"create": False, "name": "existing"},
                           "image": {"digest": digest}, "imagePullSecrets": [{"name": "registry"}],
                           "nodeSelector": {"kubernetes.io/os": "linux"},
                           "tolerations": [{"key": "dedicated", "operator": "Exists"}],
                           "affinity": {"nodeAffinity": {}},
                           "podAnnotations": {"example.com/owner": "storage"}})
        self.assertEqual({item["kind"] for item in items}, {"Deployment", "Service"})
        deployment = get(items, "Deployment")
        pod = deployment["spec"]["template"]["spec"]
        self.assertEqual(pod["serviceAccountName"], "existing")
        self.assertEqual(pod["containers"][0]["image"], "ghcr.io/zekihan/hornkeeper@" + digest)
        self.assertEqual(pod["imagePullSecrets"], [{"name": "registry"}])
        self.assertEqual(pod["nodeSelector"], {"kubernetes.io/os": "linux"})
        self.assertEqual(pod["tolerations"], [{"key": "dedicated", "operator": "Exists"}])
        self.assertEqual(pod["affinity"], {"nodeAffinity": {}})
        self.assertEqual(deployment["spec"]["template"]["metadata"]["annotations"],
                         {"example.com/owner": "storage"})

    def test_invalid_values_fail_before_installation(self):
        for values in (
            {"replicaCount": 0}, {"controller": {"leaderElection": False}},
            {"controller": {"defaultReplicas": 0}}, {"controller": {"defaultReplicas": 21}},
            {"controller": {"longhornNamespace": "INVALID"}},
            {"controller": {"defaultBackupTarget": "bad/target"}},
            {"controller": {"resyncInterval": "0s"}}, {"controller": {"resyncInterval": "-1m"}},
            {"controller": {"logLevel": "verbose"}}, {"image": {"digest": "sha256:broken"}},
            {"serviceAccount": {"create": False}}, {"controller": {"defaultReplica": 2}},
        ):
            with self.subTest(values=values):
                result = render(values)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("schema", result.stderr.lower())


if __name__ == "__main__":
    unittest.main()
