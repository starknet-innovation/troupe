"""restore

Restores the native Hermes backup troupe copied to the fixed restore path into a new VM, before
its gateway's first start: checks the archive (CRCs, unique names, SQLite quick_check), refuses
a running gateway, replaces the seeded home with the archive's (unsafe paths and symlinks
refused, runtime files skipped, _external/ entries under /home/agent), keeping Hermes's own
environment (its package manager's root folders, which align has just built and every later job
runs on: the archive's installs/ and tools/ are skipped, and its cache/ entries, the durable media
and citations Hermes's backup keeps, go into the new VM's cache/), migrates the config,
points the terminal at the guest workspace and notes the factory reset in the agent's
workstation journal. Removes the archive. Prints {"status"} or {"status", "class"}.
"""
import datetime
import importlib
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import stat
import subprocess
import sys
import zipfile

archive = importlib.import_module('archive')

HOME = Path('/home/agent/.hermes')
USER_HOME = Path('/home/agent')
WORKSPACE = '/home/agent/workspace'
RUNTIME = {'cron.pid', 'gateway.lock', 'gateway.pid', 'gateway_state.json', 'processes.json'}


class Failure(Exception):
    pass


def entries(source, home, user_home, kept):
    """Every entry to restore and its target, refusing unsafe paths and symlinks up front."""
    home, user_home = home.resolve(), user_home.resolve()
    planned = []
    for info in source.infolist():
        failure = archive.name_problem(info.filename)
        if failure:
            raise Failure(failure)
        raw = PurePosixPath(info.filename)
        if raw.name in RUNTIME or (raw.parts[0] in kept and raw.parts[0] != 'cache'):
            continue
        if raw.parts[0] == '_external':
            if len(raw.parts) == 1 or raw.parts[1:] == ('workspace',):
                continue
            # Only the workspace: anything else under the agent's home would let an old VM's
            # planted files survive the factory reset (archive.name_problem refuses it first).
            if raw.parts[1] != 'workspace':
                raise Failure('archive_external_path_not_allowed')
            base, relative = user_home, Path(*raw.parts[1:])
        else:
            base, relative = home, Path(*raw.parts)
        target = (base / relative).resolve()
        try:
            target.relative_to(base)
        except ValueError:
            raise Failure('unsafe_archive_path') from None
        mode = (info.external_attr >> 16) & 0o177777
        if stat.S_ISLNK(mode):
            raise Failure('archive_symlink')
        planned.append((info, target, mode))
    return planned


def extract(source, planned):
    for info, target, mode in planned:
        if info.is_dir():
            target.mkdir(parents=True, exist_ok=True)
            target.chmod(0o700)
            continue
        target.parent.mkdir(parents=True, exist_ok=True)
        with source.open(info) as reader, target.open('wb') as writer:
            shutil.copyfileobj(reader, writer)
        target.chmod(0o700 if mode & 0o111 else 0o600)


def set_workspace(config_path, workspace):
    import hermes_yaml as yaml  # Hermes's own YAML reader; its environment has no PyYAML
    data = yaml.safe_load(config_path.read_text(encoding='utf-8'))
    if not isinstance(data, dict) or not isinstance(data.setdefault('terminal', {}), dict):
        raise Failure('restored_config_not_a_mapping')
    data['terminal']['cwd'] = workspace
    temporary = config_path.with_name(config_path.name + '.workspace')
    temporary.write_text(yaml.safe_dump(data, sort_keys=False), encoding='utf-8')
    os.chmod(temporary, 0o600)
    os.replace(temporary, config_path)


def restore(path=None, home=HOME, user_home=USER_HOME, workspace=WORKSPACE, migrate=None, commit=None):
    path = path or archive.RESTORE
    from gateway.status import get_running_pid
    if get_running_pid() is not None:
        raise Failure('gateway_running')
    if not path.is_file() or path.stat().st_size == 0:
        raise Failure('archive_missing')
    failure = archive.problem(path)
    if failure:
        raise Failure(failure)
    if not Path(workspace).is_dir():
        raise Failure('workspace_missing')
    from hermes_cli.home_data_layout import PM_RUNTIME_ROOT_DIRS as kept
    os.umask(0o077)
    with zipfile.ZipFile(path) as source:
        # Every entry is checked before the seeded home is deleted.
        planned = entries(source, home, user_home, kept)
        home.mkdir(parents=True, exist_ok=True)
        for child in home.iterdir():
            if child.name in kept and child.is_dir() and not child.is_symlink():
                continue
            if child.is_dir() and not child.is_symlink():
                shutil.rmtree(child)
            else:
                child.unlink()
        extract(source, planned)
    if migrate is None:
        from hermes_cli.config import migrate_config
        migrate = lambda: migrate_config(interactive=False, quiet=True)
    migrate()
    set_workspace(home / 'config.yaml', workspace)
    # A factory reset gives a clean machine; the note tells the agent not to reinstall his
    # previous tools in bulk (workstation-maintenance skill).
    journal = home / 'workstation' / 'journal.md'
    journal.parent.mkdir(mode=0o700, exist_ok=True)
    commit = commit or subprocess.run(['git', '-C', '/opt/hermes', 'rev-parse', 'HEAD'], capture_output=True, text=True).stdout.strip()
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%MZ')
    with journal.open('a') as stream:
        stream.write(f'- {stamp} reset: factory reset from the image, state restored, Hermes {commit}. '
                     'Previous tools are not reinstalled.\n')
    journal.chmod(0o600)
    path.unlink()


def main(argv):
    if argv:
        print('usage: restore', file=sys.stderr)
        return 2
    try:
        restore()
    except Failure as error:
        print(json.dumps({'status': 'failed', 'class': str(error)}))
        return 1
    print(json.dumps({'status': 'passed'}))
    return 0
