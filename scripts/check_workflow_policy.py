#!/usr/bin/env python3
"""Enforce immutable Actions and credential-free build/publisher separation."""
from pathlib import Path
import re
import sys
import yaml

SHA_ACTION = re.compile(r"[\w.-]+/[\w./-]+@[0-9a-f]{40}\Z")


def check(workflow, name):
    errors = []
    def require(condition, message):
        if not condition:
            errors.append(f"{name}: {message}")
    def uses(value):
        require(value == './.github/workflows/pr.yaml' or bool(SHA_ACTION.fullmatch(value)),
                f"mutable or unsupported Action reference: {value}")
    for job_name, job in workflow.get('jobs', {}).items():
        if 'uses' in job:
            uses(job['uses'])
        for step in job.get('steps', []):
            action = step.get('uses', '')
            if action:
                uses(action)
            if action.startswith('actions/checkout@'):
                require(step.get('with', {}).get('persist-credentials') is False,
                        f'{job_name}: checkout credentials must not persist')
    if name not in ('release.yaml', 'docker_release.yaml'):
        return errors
    require(workflow.get('permissions') == {}, 'top-level permissions must be empty')
    require(not workflow.get('env'), 'no workflow-level credential environment')
    jobs = workflow['jobs']
    publisher_name = 'release' if name == 'release.yaml' else 'publish'
    for job_name, job in jobs.items():
        if job_name == publisher_name:
            expected = {'contents': 'write'} if name == 'release.yaml' else {'packages': 'write'}
            require(job.get('permissions') == expected, 'publisher permission scope changed')
            require(job.get('needs') == 'build', 'publisher must depend on build')
            require(not job.get('env'), 'publisher credentials must be step scoped')
            steps = job.get('steps', [])
            require(not any(s.get('uses', '').startswith('actions/checkout@') for s in steps),
                    'publisher must not check out or execute source')
            downloads = [s for s in steps if s.get('uses', '').startswith('actions/download-artifact@')]
            require(len(downloads) == 1 and downloads[0].get('with', {}).get('artifact-ids') ==
                    '${{ needs.build.outputs.artifact-id }}', 'download immutable build artifact ID')
            verify = [i for i, s in enumerate(steps) if s.get('run') == 'sha256sum --strict --check checksums.txt']
            require(len(verify) == 1, 'publisher must verify checksums')
            for i, step in enumerate(steps):
                if 'secrets.' in str(step) or 'github.token' in str(step):
                    require(bool(verify) and i > verify[0], 'credentials only after checksum verification')
        else:
            require(job.get('permissions') == {'contents': 'read'}, f'{job_name}: read-only permissions required')
            require('secrets.' not in str(job) and 'github.token' not in str(job),
                    f'{job_name}: build/checks must not receive publication credentials')
    require(jobs.get('build', {}).get('needs') == 'checks', 'build must be gated by checks')
    require(jobs.get('build', {}).get('outputs', {}).get('artifact-id') ==
            '${{ steps.artifact.outputs.artifact-id }}', 'build must export immutable artifact ID')
    return errors


def main():
    root = Path(__file__).resolve().parents[1]
    errors = []
    for path in sorted((root / '.github/workflows').glob('*.y*ml')):
        errors.extend(check(yaml.safe_load(path.read_text()), path.name))
    print('\n'.join(errors) if errors else 'Workflow security policy passed')
    return bool(errors)

if __name__ == '__main__':
    sys.exit(main())
