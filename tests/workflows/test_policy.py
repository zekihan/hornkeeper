"""Exercise caller boundaries; artifact verification lives in release-workflows."""
import copy
import importlib.util
from pathlib import Path
import unittest
import yaml

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('policy', ROOT / 'scripts/check_workflow_policy.py')
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)

class PolicyTests(unittest.TestCase):
    def setUp(self):
        self.workflow = yaml.safe_load((ROOT / '.github/workflows/release.yaml').read_text())

    def test_repository(self):
        for path in (ROOT / '.github/workflows').glob('*.yaml'):
            self.assertEqual(policy.check(yaml.safe_load(path.read_text()), path.name), [])

    def test_mutable_or_untrusted_workflow_is_rejected(self):
        for ref in ['zekihan/release-workflows/.github/workflows/image.yaml@main',
                    'other/repo/.github/workflows/image.yaml@' + 'a' * 40]:
            doc = copy.deepcopy(self.workflow)
            doc['jobs']['publish']['uses'] = ref
            self.assertTrue(policy.check(doc, 'release.yaml'))

    def test_missing_checks_or_image_gate_is_rejected(self):
        for job in ['publish', 'release']:
            doc = copy.deepcopy(self.workflow)
            doc['jobs'][job]['needs'] = 'decide'
            self.assertTrue(policy.check(doc, 'release.yaml'))

    def test_checks_cannot_inherit_secrets_or_write_token(self):
        for change in [{'secrets': 'inherit'}, {'permissions': {'contents': 'write'}}]:
            doc = copy.deepcopy(self.workflow)
            doc['jobs']['checks'].update(change)
            self.assertTrue(policy.check(doc, 'release.yaml'))

    def test_publication_cannot_add_source_execution_or_secrets(self):
        for change in [{'steps': [{'run': './untrusted.sh'}]}, {'secrets': 'inherit'},
                       {'permissions': {'packages': 'write', 'actions': 'write'}}]:
            doc = copy.deepcopy(self.workflow)
            doc['jobs']['publish'].update(change)
            self.assertTrue(policy.check(doc, 'release.yaml'))

    def test_publications_cannot_race(self):
        self.workflow['concurrency']['group'] += '-${{ github.ref }}'
        self.assertTrue(policy.check(self.workflow, 'release.yaml'))
