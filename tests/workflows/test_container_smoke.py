"""Exercise the container smoke check without building an image."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
REPLICA_ERROR = 'replicas must be between 1 and 20, got 0'


class ContainerSmokeTests(unittest.TestCase):
    def invoke(self, status, output):
        with tempfile.TemporaryDirectory() as directory:
            docker = Path(directory) / 'docker'
            docker.write_text('''#!/bin/sh
for argument do
  last=$argument
done
case "$1:$last" in
  build:.) ;;
  run:--version) printf '%s\\n' smoke ;;
  image:hornkeeper-smoke:local) printf '%s\\n' 25000:25000 ;;
  run:--default-replicas=0)
    printf '%s\\n' "$INVALID_OUTPUT" >&2
    exit "$INVALID_STATUS"
    ;;
  *) printf 'unexpected docker arguments: %s\\n' "$*" >&2; exit 125 ;;
esac
''')
            docker.chmod(0o755)
            env = dict(os.environ, PATH=directory + ':' + os.environ['PATH'],
                       INVALID_STATUS=str(status), INVALID_OUTPUT=output)
            return subprocess.run(['sh', str(ROOT / 'scripts/container_smoke.sh')],
                                  env=env, capture_output=True, text=True)

    def test_accepts_replica_validation_error(self):
        result = self.invoke(1, REPLICA_ERROR)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_rejects_success_for_invalid_replicas(self):
        result = self.invoke(0, '')
        self.assertNotEqual(result.returncode, 0)

    def test_rejects_unrelated_failures(self):
        for status, output in (
            (125, 'docker: could not start container'),
            (1, 'load Kubernetes configuration: no configuration has been provided'),
            (2, REPLICA_ERROR),
        ):
            with self.subTest(status=status):
                result = self.invoke(status, output)
                self.assertNotEqual(result.returncode, 0, result.stderr)
                self.assertIn(output, result.stderr)
