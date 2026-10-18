"""backup make | backup clean LOCK_EXISTED

make: Hermes's own native backup of the stopped (or, for the recovery point, running) agent's
state into the fixed path troupe streams out, private to the agent, with every entry checked
(CRC, names the restore accepts, SQLite integrity). Prints {"status", "lock_existed",
"commit", "sha256", "size"}: whether Hermes's backup lock existed before (the backup leaves
one behind), the Hermes commit the state belongs to, and the archive's digest and size.
clean: removes the copy and, when it did not exist before, the backup lock.
"""
import hashlib
import importlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

archive = importlib.import_module('archive')

HOME = Path('/home/agent/.hermes')
HERMES = '/home/agent/.local/bin/hermes'


def make(home=HOME, path=None, hermes=HERMES):
    path = path or archive.BACKUP
    existed = (home / '.backup.lock').exists()
    path.unlink(missing_ok=True)
    path.parent.mkdir(parents=True, exist_ok=True)
    result = subprocess.run([hermes, 'backup', '-o', str(path)], stdin=subprocess.DEVNULL,
                            stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
    if result.returncode or not path.is_file() or path.stat().st_size == 0:
        return {'status': 'failed', 'class': 'native_backup_failed'}
    os.chmod(path, 0o600)
    failure = archive.problem(path)
    if failure:
        return {'status': 'failed', 'class': failure}
    commit = subprocess.run(['git', '-C', '/opt/hermes', 'rev-parse', 'HEAD'], capture_output=True, text=True).stdout.strip()
    if not re.fullmatch(r'[0-9a-f]{40}', commit):
        return {'status': 'failed', 'class': 'hermes_commit_unreadable'}
    # The host checks only that it received these exact bytes; every content check is here.
    digest = hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b''):
            digest.update(chunk)
    return {'status': 'passed', 'lock_existed': existed, 'commit': commit,
            'sha256': digest.hexdigest(), 'size': path.stat().st_size}


def clean(lock_existed, home=HOME, path=None):
    (path or archive.BACKUP).unlink(missing_ok=True)
    if not lock_existed:
        (home / '.backup.lock').unlink(missing_ok=True)
    return {'status': 'passed'}


def main(argv):
    if argv == ['make']:
        result = make()
    elif len(argv) == 2 and argv[0] == 'clean' and argv[1] in ('0', '1'):
        result = clean(argv[1] == '1')
    else:
        print('usage: backup make | backup clean 0|1', file=sys.stderr)
        return 2
    print(json.dumps(result))
    return 0 if result['status'] == 'passed' else 1
