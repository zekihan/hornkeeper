#!/usr/bin/env python3
"""Validate pinned publishers, native-check gates, and credential boundaries."""
from pathlib import Path
import re
import sys
import yaml

SHA_ACTION = re.compile(r'[\w.-]+/[\w./-]+@[0-9a-f]{40}\Z')
SHARED = 'zekihan/release-workflows/.github/workflows/'


def check(workflow, name):
    errors = []
    def require(condition, message):
        if not condition:
            errors.append(f'{name}: {message}')
    require(workflow.get('permissions') == {'contents': 'read'}, 'workflow token must be read-only')
    require(not workflow.get('env'), 'no workflow-level credential environment')
    for job_name, job in workflow.get('jobs', {}).items():
        require(not job.get('env'), f'{job_name}: no job-level credential environment')
        for entry in [job, *job.get('steps', [])]:
            action = entry.get('uses', '')
            if action:
                require(action == './.github/workflows/pr.yaml' or bool(SHA_ACTION.fullmatch(action)),
                        f'{job_name}: use reviewed immutable Actions')
            if action.startswith('actions/checkout@'):
                require(entry.get('with', {}).get('persist-credentials') is False, 'disable checkout credentials')
    if name != 'release.yaml':
        return errors
    jobs = workflow['jobs']
    require(set(jobs) == {'decide', 'checks', 'publish', 'release'}, 'unexpected publication jobs')
    expected = {'decide': ('decide.yaml', {'contents': 'read', 'actions': 'read', 'packages': 'read'}),
                'publish': ('image.yaml', {'contents': 'read', 'packages': 'write'}),
                'release': ('release.yaml', {'contents': 'write', 'packages': 'write'})}
    pins = set()
    for job_name, (file, permissions) in expected.items():
        job = jobs.get(job_name, {})
        require(bool(re.fullmatch(re.escape(SHARED + file) + '@[0-9a-f]{40}', job.get('uses', ''))),
                f'{job_name}: use the pinned shared publisher')
        pins.add(job.get('uses', '').split('@')[-1])
        require(job.get('permissions') == permissions, f'{job_name}: unexpected token scope')
        require('steps' not in job, f'{job_name}: no additional credentialed execution')
        require(job.get('secrets') == {key: '${{ secrets.' + key + ' }}' for key in ['DOCKER_USERNAME', 'DOCKER_PASSWORD']},
                f'{job_name}: pass only explicit registry secrets')
    require(len(pins) == 1, 'shared workflow pins must agree')
    checks = jobs.get('checks', {})
    require(checks.get('uses') == './.github/workflows/pr.yaml', 'use local native checks')
    require(checks.get('permissions') == {'contents': 'read'} and 'secrets' not in checks, 'checks are credential-free')
    require(checks.get('needs') == 'decide', 'decide before checks')
    require(checks.get('with') == {'publishing': True}, 'publication includes smoke checks')
    require(jobs.get('publish', {}).get('needs') == ['decide', 'checks'], 'checks gate images')
    require(jobs.get('release', {}).get('needs') == ['decide', 'checks', 'publish'], 'verified images gate releases')
    require(workflow.get('concurrency') == {'group': 'publish-${{ github.repository }}', 'cancel-in-progress': False},
            'serialize channel promotions')
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
