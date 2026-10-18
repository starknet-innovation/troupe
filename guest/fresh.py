"""fresh

A VM just created from its kits holds nothing from before and matches its declaration: no
gateway running yet, an empty workspace, no sessions, memories or database records, no
backup but the seeded config's, empty Docker storage, the declared CPUs and memory, and the
declared disks and mounts (storage.py, also the storage job). Reads request.json:

    {"cpus": 8, "memory_gib": 12, "config_sha256": "...", "root_gib": 200, "docker_gib": 50}

Prints {"status": "passed"} or {"status": "failed", "class": ...}.
"""
import hashlib
import importlib
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import sys

storage = importlib.import_module('storage')

HOME = Path('/home/agent/.hermes')
WORKSPACE = Path('/home/agent/workspace')


class Failure(Exception):
    pass


def require(value, reason):
    if not value:
        raise Failure(reason)


def check(request, home=HOME, workspace=WORKSPACE, meminfo=Path('/proc/meminfo'), docker=None):
    from gateway.status import get_running_pid
    require(get_running_pid() is None, 'gateway_started_before_initialization')
    require(os.cpu_count() == request['cpus'], 'cpu_count_mismatch')
    total = next(int(line.split()[1]) for line in meminfo.read_text().splitlines() if line.startswith('MemTotal:'))
    declared = request['memory_gib'] * 1024 * 1024  # KiB
    require(declared * 0.95 <= total <= declared, 'memory_mismatch')
    workspace.mkdir(mode=0o700, exist_ok=True)
    require(not any(workspace.iterdir()), 'workspace_not_empty')
    for name in ('state.db', 'shared-state.db', 'kanban.db'):
        path = home / name
        if path.exists():
            db = sqlite3.connect('file:' + str(path) + '?mode=ro', uri=True)
            try:
                tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'")}
                for table in ('sessions', 'messages', 'tasks'):
                    if table in tables:
                        require(db.execute('SELECT count(*) FROM ' + table).fetchone()[0] == 0, 'old_records_present')
            finally:
                db.close()
    for name in ('sessions', 'memories'):
        path = home / name
        require(not path.exists() or not any(path.iterdir()), 'old_state_present')
    backups = home / 'backups'
    require(not backups.is_symlink(), 'unexpected_backup_link')
    if backups.exists():
        for path in backups.rglob('*'):
            relative = path.relative_to(backups).as_posix()
            require(not path.is_symlink(), 'unexpected_backup_link')
            if relative == 'config' and path.is_dir():
                continue
            require(re.fullmatch(r'config/config\.yaml\.good\.[0-9]{8}-[0-9]{6}', relative), 'unexpected_backup_present')
            require(path.is_file() and hashlib.sha256(path.read_bytes()).hexdigest() == request['config_sha256'],
                    'nonseed_backup_present')
    if request['docker_gib']:
        for args in (['ps', '-aq'], ['volume', 'ls', '-q'], ['image', 'ls', '-q']):
            out = (docker or (lambda a: subprocess.check_output(['docker', *a])))(args)
            require(not out.strip(), 'docker_storage_not_empty')


def main(argv):
    if argv:
        print("usage: fresh", file=sys.stderr)
        return 2
    request = json.loads((Path(__file__).resolve().parent / 'request.json').read_text())
    try:
        check(request)
        storage.check(request['root_gib'], request['docker_gib'])
    except Failure as error:
        print(json.dumps({"status": "failed", "class": str(error)}))
        return 1
    except AssertionError as error:
        print(json.dumps({"status": "failed", "class": str(error).rpartition("class=")[2]}))
        return 1
    print(json.dumps({"status": "passed"}))
    return 0
