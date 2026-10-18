"""align COMMIT

Brings a new VM's Hermes up to the commit the preserved state belongs to, before that state is
restored: the agent's own updates already migrated his state, so the image's older checkout
cannot run it. When the image's checkout already contains the preserved commit, nothing is
updated. Otherwise Hermes has no exact-commit install, so this takes Hermes's native update to
current main (skipping its pre-update backup, since this VM's state is replaced next) and
proves the preserved commit is contained in the result. Prints {"status", "commit", "updated"}.
"""
import json
import re
import subprocess
import sys

HERMES = '/home/agent/.local/bin/hermes'


def align(commit, run=subprocess.run):
    from gateway.status import get_running_pid
    if get_running_pid() is not None:
        return {'status': 'failed', 'class': 'gateway_running'}
    def contained():
        return run(['git', '-C', '/opt/hermes', 'merge-base', '--is-ancestor', commit, 'HEAD'],
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
    updated = False
    if not contained():
        update = run([HERMES, 'update', '--yes', '--no-gateway-restart', '--no-backup'],
                     stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if update.returncode:
            return {'status': 'failed', 'class': 'hermes_update_failed'}
        if not contained():
            return {'status': 'failed', 'class': 'preserved_commit_not_contained'}
        updated = True
    head = run(['git', '-C', '/opt/hermes', 'rev-parse', 'HEAD'], capture_output=True, text=True).stdout.strip()
    return {'status': 'passed', 'commit': head, 'updated': updated}


def main(argv):
    if len(argv) != 1 or not re.fullmatch(r'[0-9a-f]{40}', argv[0]):
        print('usage: align COMMIT', file=sys.stderr)
        return 2
    result = align(argv[0])
    print(json.dumps(result))
    return 0 if result['status'] == 'passed' else 1
