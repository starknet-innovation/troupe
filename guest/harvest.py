"""harvest

The guest half of troupe harvest (dev build): the live copies of the agent's seeded skill files, the
native curator ledger's agent entries and the skill files the agent created. The request is
request.json beside this file ({"paths": [...]}, relative to ~/.hermes); the answer is one JSON
line. It only reads.
"""
import base64
import hashlib
import json
import os
from pathlib import Path
import sys

# Tests point this at a temporary home; in a VM it is always the agent's ~/.hermes.
HOME = Path(os.environ.get('TROUPE_TEST_HERMES_HOME', '/home/agent/.hermes'))


def collect(request, home=HOME):
    def read(relative):
        path = home / relative
        if not path.is_file():
            return None
        data = path.read_bytes()
        return {'sha256': hashlib.sha256(data).hexdigest(), 'b64': base64.b64encode(data).decode()}

    files = {relative: read(relative) for relative in request['paths']}
    entries, created = [], {}
    ledger = home / 'skills/.curator_ledger.jsonl'
    for line in (ledger.read_text().splitlines() if ledger.is_file() else []):
        try:
            entry = json.loads(line)
        except ValueError:
            continue
        if not isinstance(entry, dict) or entry.get('actor') != 'agent':
            continue
        # Each path keeps its own hash: one outside the home is dropped with it, never shifting the rest.
        after = [a for a in entry.get('after') or [] if isinstance(a, dict) and isinstance(a.get('path'), str)
                 and a['path'].startswith(str(home) + '/')]
        paths = [str(Path(a['path']).relative_to(home)) for a in after]
        entries.append({'ts': entry.get('ts'), 'action': entry.get('action'), 'skill': entry.get('skill'),
                        'paths': paths, 'after': [a.get('sha256') for a in after]})
        for relative in paths:
            if relative not in files and relative not in created:
                created[relative] = read(relative)
    return {'status': 'passed', 'files': files, 'ledger': entries, 'created': created}


def main(argv):
    if argv:
        print('usage: harvest (request in request.json)', file=sys.stderr)
        return 2
    request = json.loads((Path(__file__).resolve().parent / 'request.json').read_text())
    print(json.dumps(collect(request)))
    return 0
