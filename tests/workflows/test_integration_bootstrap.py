"""Exercise the integration entrypoint without starting a real API server."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class IntegrationBootstrapTests(unittest.TestCase):
    def invoke(self, assets='', bootstrap_exit='0', test_exit='0'):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fake = root / 'go'
            fake.write_text('''#!/bin/sh
printf '%s|assets=%s\\n' "$*" "${KUBEBUILDER_ASSETS:-}" >> "$CALLS"
if [ "$1" = run ]; then
  test "$BOOTSTRAP_EXIT" = 0 || exit "$BOOTSTRAP_EXIT"
  printf '%s\\n' '/fixture/newly-downloaded-assets'
else
  exit "$TEST_EXIT"
fi
''')
            fake.chmod(0o755)
            env = dict(os.environ, PATH=str(root)+':'+os.environ['PATH'], CALLS=str(root/'calls'),
                       KUBEBUILDER_ASSETS=assets, BOOTSTRAP_EXIT=bootstrap_exit, TEST_EXIT=test_exit,
                       ENVTEST_BIN_DIR=str(root/'empty-cache'))
            result = subprocess.run(['sh', str(ROOT/'scripts/integration.sh')], env=env,
                                    capture_output=True, text=True)
            return result.returncode, (root/'calls').read_text().splitlines()

    def test_fresh_bootstrap_is_immutable_verified_and_runs_full_gate(self):
        status, calls = self.invoke()
        self.assertEqual(0, status)
        self.assertEqual(2, len(calls))
        self.assertIn('setup-envtest@5d8467b3065818a3b382d8f6941147a801fd1969', calls[0])
        self.assertIn('--verify=true', calls[0])
        self.assertIn('/1031496fc98a4f51010c3bdfdeb57b5d67bea7bd/envtest-releases.yaml', calls[0])
        self.assertIn('-p path 1.37.0', calls[0])
        self.assertIn('test -race -count=1 -tags=integration', calls[1])
        self.assertIn('TestDisposableAPIServer|TestExamplePermissionsAndLeaderFailover|TestStartupWithBundledRBAC', calls[1])
        self.assertTrue(calls[1].endswith('assets=/fixture/newly-downloaded-assets'))

    def test_failed_download_does_not_run_tests(self):
        status, calls = self.invoke(bootstrap_exit='7')
        self.assertEqual(7, status)
        self.assertEqual(1, len(calls))

    def test_explicit_assets_still_run_tests(self):
        status, calls = self.invoke(assets='/fixture/operator-assets')
        self.assertEqual(0, status)
        self.assertEqual(1, len(calls))
        self.assertTrue(calls[0].startswith('test -race'))
        self.assertTrue(calls[0].endswith('assets=/fixture/operator-assets'))

    def test_test_failure_is_not_hidden(self):
        status, calls = self.invoke(test_exit='9')
        self.assertEqual(9, status)
        self.assertEqual(2, len(calls))
