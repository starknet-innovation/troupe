"""troupe's guest jobs, run inside an agent's VM on Hermes's own Python, as the agent's own user:

    python -B <install dir> JOB [ARGS...]

The host sends this folder with every call (troupe internal/guest) and never runs it itself. A job prints
its result as one JSON line on stdout, with "status" passed or failed and a failure "class";
a failed job exits 1.

A job reports what the VM shows. That catches misconfiguration, but it is no proof against a
compromised agent, which controls the VM it runs in; only host-side facts are.
"""
import importlib
from pathlib import Path
import re
import sys

# Job name -> module in this folder. A job of troupe's dev build is its module's own name, and
# runs only where that module was shipped; HELPERS are modules that are never jobs.
JOBS = {'align': 'align', 'backup': 'backup', 'drain': 'drain', 'fresh': 'fresh',
        'gateway-stopped': 'stopped', 'kit': 'kit', 'ready': 'ready',
        'restore': 'restore', 'selftest': 'selftest', 'slack': 'slack', 'stop-gateway': 'stop',
        'storage': 'storage', 'update': 'update'}
HELPERS = {'archive', 'install'}
HERE = Path(__file__).resolve().parent
# Hermes's own source, whose gateway modules the jobs call.
HERMES = '/opt/hermes'


def module(job):
    """The module that runs job, or None."""
    if job in JOBS:
        return JOBS[job]
    if re.fullmatch(r'[a-z]+', job) and job not in JOBS.values() and job not in HELPERS and (HERE / f'{job}.py').is_file():
        return job
    return None


def main(argv):
    name = module(argv[0]) if argv else None
    if name is None:
        print(f"usage: JOB [ARGS...], JOB one of: {', '.join(sorted(JOBS))}", file=sys.stderr)
        return 2
    if HERMES not in sys.path:
        sys.path.insert(1, HERMES)
    return importlib.import_module(name).main(argv[1:])


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
