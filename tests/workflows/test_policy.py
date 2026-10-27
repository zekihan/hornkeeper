import copy
import json
import os
import shutil
import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
import yaml

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('policy', ROOT / 'scripts/check_workflow_policy.py')
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)


class PolicyTests(unittest.TestCase):
    def setUp(self):
        self.name = 'docker_release.yaml'
        self.workflow = yaml.safe_load((ROOT / '.github/workflows' / self.name).read_text())

    def test_repository_workflows(self):
        for path in (ROOT / '.github/workflows').glob('*.yaml'):
            self.assertEqual([], policy.check(yaml.safe_load(path.read_text()), path.name))

    def test_mutable_actions_rejected_including_nonprivileged_steps(self):
        for ref in ('actions/checkout@v7.0.1', 'actions/checkout@main', 'actions/checkout@1234567',
                    'docker://alpine:latest', './unreviewed-action'):
            with self.subTest(ref=ref):
                data = copy.deepcopy(self.workflow)
                data['jobs']['build']['steps'][0]['uses'] = ref
                self.assertTrue(policy.check(data, self.name))

    def test_build_write_token_rejected(self):
        self.workflow['jobs']['build']['permissions']['packages'] = 'write'
        self.assertTrue(policy.check(self.workflow, self.name))

    def test_build_credentials_rejected(self):
        self.workflow['jobs']['build']['env'] = {'TOKEN': '${{ secrets.DOCKER_PASSWORD }}'}
        self.assertTrue(policy.check(self.workflow, self.name))

    def test_name_based_artifact_download_rejected(self):
        self.workflow['jobs']['publish']['steps'][0]['with'] = {'name': 'container-and-chart'}
        self.assertTrue(policy.check(self.workflow, self.name))

    def test_missing_checksum_rejected(self):
        del self.workflow['jobs']['publish']['steps'][1]
        self.assertTrue(policy.check(self.workflow, self.name))

    def test_early_credentials_rejected(self):
        self.workflow['jobs']['publish']['steps'][0]['env'] = {'TOKEN': '${{ github.token }}'}
        self.assertTrue(policy.check(self.workflow, self.name))

    def test_persistent_checkout_rejected(self):
        self.workflow['jobs']['build']['steps'][0]['with']['persist-credentials'] = True
        self.assertTrue(policy.check(self.workflow, self.name))

    def test_publisher_checkout_rejected(self):
        self.workflow['jobs']['publish']['steps'].append(self.workflow['jobs']['build']['steps'][0])
        self.assertTrue(policy.check(self.workflow, self.name))

    def test_actual_verification_command_accepts_valid_and_rejects_tampered_artifact(self):
        command = self.workflow['jobs']['publish']['steps'][1]['run']
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'image.tar').write_bytes(b'fixture image')
            subprocess.run('sha256sum image.tar > checksums.txt', shell=True, cwd=root, check=True)
            self.assertEqual(0, subprocess.run(command, shell=True, cwd=root, capture_output=True).returncode)
            (root / 'image.tar').write_bytes(b'tampered')
            self.assertNotEqual(0, subprocess.run(command, shell=True, cwd=root, capture_output=True).returncode)

    def test_container_publisher_uses_only_verified_archive_and_cleans_credentials(self):
        command = self.workflow['jobs']['publish']['steps'][-1]['run']
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            bin_dir = root / 'bin'
            bin_dir.mkdir()
            for tool in ('skopeo', 'helm'):
                script = bin_dir / tool
                script.write_text('#!/bin/sh\nprintf "%s\\n" "$0 $*" >> "$CALLS"\ncase "$*" in *login*) cat >/dev/null;; esac\n')
                script.chmod(0o755)
            env = dict(os.environ, PATH=str(bin_dir) + ':' + os.environ['PATH'], CALLS=str(root/'calls'),
                       RUNNER_TEMP=str(root), DOCKER_USERNAME='fixture', DOCKER_PASSWORD='fake-password',
                       GITHUB_TOKEN='fake-token', REGISTRY_USER='fixture', REPOSITORY='fixture/hornkeeper',
                       REPOSITORY_OWNER='fixture', VERSION='1.2.3', COMMIT='abcdef1234567890', GITHUB_REF='refs/tags/v1.2.3')
            result = subprocess.run([('/opt/homebrew/bin/bash' if Path('/opt/homebrew/bin/bash').exists() else shutil.which('bash')), '-c', command], env=env, cwd=root, capture_output=True, text=True)
            self.assertEqual(0, result.returncode, result.stderr)
            calls = (root/'calls').read_text()
            copies = [line for line in calls.splitlines() if ' copy ' in line]
            self.assertEqual(8, len(copies), calls)
            self.assertTrue(all('copy --all --preserve-digests oci-archive:dist/image.tar docker://' in line for line in copies))
            self.assertIn('helm push dist/hornkeeper-1.2.3.tgz oci://ghcr.io/fixture/charts', calls)
            self.assertNotIn('fake-password', calls)
            self.assertNotIn('fake-token', calls)
            self.assertFalse((root/'registry-auth.json').exists())
            self.assertFalse((root/'helm-registry.json').exists())

    def test_release_publisher_uses_checksum_verified_assets(self):
        workflow = yaml.safe_load((ROOT/'.github/workflows/release.yaml').read_text())
        command = workflow['jobs']['release']['steps'][-1]['run']
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root/'fixture.tar.gz').write_text('fixture')
            (root/'checksums.txt').write_text('fixture')
            script = root/'gh'
            script.write_text('#!/bin/sh\nprintf "%s\\n" "$*" > "$CALLS"\n')
            script.chmod(0o755)
            env = dict(os.environ, PATH=str(root)+':'+os.environ['PATH'], CALLS=str(root/'calls'), RELEASE_TAG='v1.2.3')
            subprocess.run(['bash', '-c', command], cwd=root, env=env, check=True)
            self.assertEqual('release create v1.2.3 --verify-tag --generate-notes -- fixture.tar.gz checksums.txt', (root/'calls').read_text().strip())

    def test_action_updates_remain_pinned_and_reviewed(self):
        config = json.loads((ROOT/'renovate.json').read_text())
        rules = [rule for rule in config['packageRules'] if 'github-actions' in rule.get('matchManagers', [])]
        self.assertTrue(rules[-1]['pinDigests'])
        self.assertIs(rules[-1]['automerge'], False)
