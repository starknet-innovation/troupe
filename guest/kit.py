"""kit TOOL ACTION

Runs one of the base kit's own tools, sent beside the jobs under kit/, on Hermes's Python:

    python -B kit/TOOL ACTION

with TROUPE_SEEDS naming the agent's composed seeds (sent under seeds/) and TROUPE_REQUEST
naming request.json. The tool reports through its exit status; its output is returned for
the host's private log, and a failure's class is taken from a `class=NAME` it prints.
Prints {"status", "class", "output"}.
"""
import json
import os
from pathlib import Path
import re
import subprocess
import sys

HERE = Path(__file__).resolve().parent
LIMIT = 65536


def main(argv):
    if len(argv) != 2 or not re.fullmatch(r'[a-z][a-z0-9-]*\.py', argv[0]) or not re.fullmatch(r'[a-z][a-z0-9-]*', argv[1]):
        print("usage: kit TOOL ACTION", file=sys.stderr)
        return 2
    tool = HERE / 'kit' / argv[0]
    if not tool.is_file():
        print(json.dumps({"status": "failed", "class": "kit_tool_missing", "output": ""}))
        return 1
    env = dict(os.environ, TROUPE_SEEDS=str(HERE / 'seeds'), TROUPE_REQUEST=str(HERE / 'request.json'))
    result = subprocess.run([sys.executable, '-B', str(tool), argv[1]], env=env,
                            stdin=subprocess.DEVNULL, capture_output=True, text=True, errors='replace')
    output = (result.stdout + result.stderr)[-LIMIT:]
    if result.returncode:
        found = re.findall(r'class=([a-z][a-z0-9_]*)', output)
        print(json.dumps({"status": "failed", "class": found[-1] if found else "kit_tool_failed", "output": output}))
        return 1
    print(json.dumps({"status": "passed", "output": output}))
    return 0
