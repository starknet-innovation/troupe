"""Checks shared by the backup and restore jobs (not a job itself)."""
import sqlite3
import tempfile
import zipfile
from contextlib import closing
from pathlib import Path

# The fixed paths a migrate copies state through; jobs accept no other.
BACKUP = Path('/home/agent/.local/state/hermes-replacement-backup.zip')
RESTORE = Path('/home/agent/.local/state/hermes-replacement-state.zip')


def name_problem(name):
    """'' when the restore can write this entry exactly where its name says; otherwise the class.

    Checked with the same zipfile parser the restore uses, so no other reader's view matters:
    no absolute, parent, empty, dot or doubled path parts, and under _external/ only the
    workspace (anything else under the agent's home would survive a factory reset)."""
    stripped = name.rstrip('/')
    parts = stripped.split('/')
    if (not stripped or name.startswith('/') or '\x00' in name or '\\' in name
            or any(part in ('', '.', '..') for part in parts)):
        return 'unsafe_archive_path'
    if parts[0] == '_external' and len(parts) > 1 and parts[1] != 'workspace':
        return 'archive_external_path_not_allowed'
    return ''


def problem(path):
    """'' when every entry passes its CRC, no name repeats, every name is one the restore writes
    exactly, and every SQLite database passes quick_check; otherwise the failure class."""
    try:
        with zipfile.ZipFile(path) as source, tempfile.TemporaryDirectory(prefix='troupe-archive-') as tmp:
            names = source.namelist()
            for name in names:
                failure = name_problem(name)
                if failure:
                    return failure
            if source.testzip() is not None or len(names) != len(set(names)):
                return 'archive_invalid'
            for name in names:
                if not name.endswith('.db'):
                    continue
                db = Path(tmp) / 'database.db'
                db.write_bytes(source.read(name))
                db.chmod(0o600)
                with closing(sqlite3.connect(db.resolve().as_uri() + '?mode=ro', uri=True, timeout=5)) as conn:
                    if conn.execute('PRAGMA quick_check').fetchall() != [('ok',)]:
                        return 'sqlite_integrity_failed'
                db.unlink()
    except (zipfile.BadZipFile, sqlite3.DatabaseError, OSError):
        return 'archive_invalid'
    return ''
