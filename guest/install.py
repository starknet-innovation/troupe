"""update install: the seed writer of troupe upgrade's seed step and the dev build's troupe update."""
import base64
import os
from pathlib import Path
import tempfile

from update import HOME, digest, linked


def install(request, home=HOME):
    """Write and delete exactly the planned files, or nothing: every live file must still have the
    hash it had when the change list was shown, and any failure restores what was there."""
    def current(p):
        return digest(p.read_bytes()) if p.is_file() and not p.is_symlink() else None

    def atomic(p, data):
        for parent in reversed(p.parents):
            if home in parent.parents and not parent.exists():
                parent.mkdir(mode=0o700)
        if p.is_symlink():
            raise RuntimeError('destination_is_symlink')
        fd, name = tempfile.mkstemp(prefix='.troupe-update-', dir=p.parent)
        try:
            with os.fdopen(fd, 'wb') as stream:
                os.fchmod(stream.fileno(), 0o600)
                stream.write(data)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(name, p)
        finally:
            if os.path.exists(name):
                os.unlink(name)

    for rel in request['files']:
        if Path(rel).is_absolute() or '..' in Path(rel).parts:
            return {'status': 'failed', 'class': 'path_not_allowed', 'path': rel}
    for rel, item in request['files'].items():
        if current(home / rel) != item['before']:
            return {'status': 'failed', 'class': 'live_file_changed', 'path': rel}
    written = []
    try:
        for rel, item in request['files'].items():
            p = home / rel
            if linked(home, p.parent):
                raise RuntimeError('parent_symlink')
            written.append(rel)
            if item['after'] is None:
                if p.exists() or p.is_symlink():
                    p.unlink()
            else:
                data = base64.b64decode(item['b64'])
                if digest(data) != item['after']:
                    raise RuntimeError('payload_hash_mismatch')
                atomic(p, data)
        if request.get('fail_after_first_write'):
            raise RuntimeError('forced_failure')
    except Exception as exc:
        restored = True
        for rel in reversed(written):
            original = request['files'][rel]['original_b64']
            p = home / rel
            try:
                if original is None:
                    p.unlink(missing_ok=True)
                else:
                    atomic(p, base64.b64decode(original))
            except Exception:
                restored = False  # say so: the host must not claim live is unchanged
        return {'status': 'failed', 'class': str(exc) if isinstance(exc, RuntimeError) else type(exc).__name__,
                'restored': restored}
    # Folders a deletion emptied go too, deepest first, never ~/.hermes's own top folders.
    for folder in sorted({str(Path(rel).parent) for rel, item in request['files'].items() if item['after'] is None},
                         key=len, reverse=True):
        p = home / folder
        try:
            while p != home and p.parent != home and p.is_dir() and not p.is_symlink() and not any(p.iterdir()):
                p.rmdir()
                p = p.parent
        except OSError:
            pass  # a leftover empty folder is harmless; the files are already right
    return {'status': 'passed'}
