"""update read|state|signal|install

The guest half of troupe restart, troupe upgrade's seed step and the dev build's troupe reseed
(install.py writes the seeds). The request is request.json beside this file (it can carry whole files, so
it never travels on the command line); the answer is one JSON line. Paths are relative to
~/.hermes; nothing outside it is read or written.
"""
import base64
import hashlib
import importlib
import json
import os
from pathlib import Path
import sys

# Tests point this at a temporary home; in a VM it is always the agent's ~/.hermes.
HOME = Path(os.environ.get('TROUPE_TEST_HERMES_HOME', '/home/agent/.hermes'))


def digest(data):
    return hashlib.sha256(data).hexdigest()


def linked(home, path):
    """True when path or a parent below home is a symbolic link."""
    return any(q.is_symlink() for q in [path, *path.parents] if q != home and home in q.parents)


def read(request, home=HOME):
    """Every live file under the requested folders and files, with the gateway and running work."""
    out: dict = {'files': {}, 'running': [], 'gateway_pid': None}

    def add(rel):
        p = home / rel
        if linked(home, p):
            out['files'][rel] = {'error': 'symlink'}
        elif p.is_file():
            out['files'][rel] = {'b64': base64.b64encode(p.read_bytes()).decode()}
        elif p.exists():
            out['files'][rel] = {'error': 'not_regular_file'}

    for folder in request.get('folders', []):
        root = home / folder
        if linked(home, root):
            out['files'][folder] = {'error': 'symlink'}
            continue
        if root.is_dir():
            for path in sorted(root.rglob('*')):
                if '__pycache__' in path.parts or path.is_dir() and not path.is_symlink():
                    continue
                add(path.relative_to(home).as_posix())
    for rel in request.get('files', []):
        add(rel)
    try:
        from gateway.status import get_running_pid
        out['gateway_pid'] = get_running_pid()
        from cron import executions
        out['running'] = [{k: e.get(k) for k in ('id', 'job_id', 'status', 'started_at')}
                          for e in executions.list_executions(limit=50) if e.get('status') in ('claimed', 'running')]
    except Exception as exc:
        out['runtime_error'] = type(exc).__name__
    return out


def state(request, home=HOME):
    """Installed hashes and modes, and on request the runtime, a new session's skill index,
    the status of executions and the forced-drain log lines since a time."""
    out: dict = {'files': {}}
    for rel in request.get('files', []):
        p = home / rel
        if p.is_file() and not p.is_symlink():
            st = p.stat()
            out['files'][rel] = {'sha256': digest(p.read_bytes()), 'mode': oct(st.st_mode & 0o777),
                                 'uid': st.st_uid, 'parent_mode': oct(p.parent.stat().st_mode & 0o777)}
        else:
            out['files'][rel] = None
    if request.get('since') is not None:
        import sqlite3
        c = sqlite3.connect('file:' + str(home / 'state.db') + '?mode=ro', uri=True)
        row = c.execute('select s.id, s.source, s.started_at, p.prompt from sessions s join system_prompts p '
                        'on p.hash = s.system_prompt_hash where s.started_at > ? order by s.started_at limit 1',
                        (request['since'],)).fetchone()
        out['session'] = None if row is None else {'source': row[1], 'started_at': row[2], 'prompt': row[3]}
    if request.get('executions') is not None:
        from cron import executions
        out['executions'] = {i: (executions.get_execution(i) or {}).get('status') for i in request['executions']}
    if request.get('log_since') is not None:
        import datetime
        forced = 0
        log = home / 'logs/gateway.log'
        for line in (log.read_text(errors='replace').splitlines() if log.is_file() else []):
            if 'Skipping .clean_shutdown marker' not in line:
                continue
            try:
                stamp = datetime.datetime.strptime(line[:19], '%Y-%m-%d %H:%M:%S').replace(
                    tzinfo=datetime.timezone.utc).timestamp()
            except ValueError:
                continue
            forced += stamp >= request['log_since']
        out['forced_drains'] = forced
    if request.get('runtime'):
        from gateway.status import get_running_pid, read_runtime_status
        status = read_runtime_status() or {}
        out['runtime'] = {'pid': get_running_pid(), 'gateway_state': status.get('gateway_state'),
                          'platforms': {k: (v or {}).get('state') for k, v in (status.get('platforms') or {}).items()},
                          'restart_requested': status.get('restart_requested')}
    return out


def signal(request):
    """Hermes's graceful restart: refuse new turns, finish active work, exit 75 for the unit."""
    import signal as signals
    from gateway.status import get_running_pid
    pid = get_running_pid()
    if not pid or pid != request['pid']:
        return {'status': 'failed', 'class': 'gateway_pid_changed'}
    os.kill(pid, signals.SIGUSR1)
    return {'status': 'passed', 'pid': pid}


ACTIONS = {'read': read, 'state': state, 'signal': signal}


def actions():
    """The actions this build ships, with install.py's loaded only when asked for (it imports
    the helpers above)."""
    found = dict(ACTIONS)
    if (Path(__file__).resolve().parent / 'install.py').is_file():
        found['install'] = lambda request: importlib.import_module('install').install(request)
    return found


def main(argv):
    shipped = actions()
    if len(argv) != 1 or argv[0] not in shipped:
        print(f"usage: update {'|'.join(shipped)} (request in request.json)", file=sys.stderr)
        return 2
    request = json.loads((Path(__file__).resolve().parent / 'request.json').read_text())
    result = shipped[argv[0]](request)
    print(json.dumps(result))
    return 1 if result.get('status') == 'failed' else 0
