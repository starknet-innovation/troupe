"""selftest

One model turn through the agent's own Hermes and provider, as a new agent's last step:
`hermes -z PROMPT --usage-file FILE`, judged on transport only (the configured model in the
usage record, at least one API call, output tokens, the turn completed), never on what the
model says. One retry. Prints {"status": "passed", "reply": TEXT} or {"status": "failed",
"class": CLASS}, the class one of key_rejected, network_unreachable, timeout, provider_failed,
usage_record_invalid or config_unreadable.
"""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

HERMES = '/home/agent/.local/bin/hermes'
WORKSPACE = '/home/agent/workspace'
HOME = Path(os.environ.get('HERMES_HOME', '/home/agent/.hermes'))
PROMPT = 'Reply in one short sentence that you are ready.'
TIMEOUT = 300
REPLY_LIMIT = 500
# Fixed matches on Hermes's error output, in order: a refused key, then an unreachable network.
KEY = re.compile(r'\b(401|403)\b|unauthori[sz]ed|forbidden|invalid api key|authentication', re.I)
NETWORK = re.compile(r'name or service not known|temporary failure in name resolution|connection (refused|reset|error)|'
                     r'network is unreachable|could not resolve|failed to establish', re.I)


def configured_model(root=HOME):
    import hermes_yaml as yaml  # Hermes's own YAML reader; its environment has no PyYAML
    config = yaml.safe_load((root / 'config.yaml').read_text()) or {}
    model = (config.get('model') or {}).get('default')
    return model if isinstance(model, str) and model else None


def classify(code, stderr):
    if code == 124:
        return 'timeout'
    if KEY.search(stderr):
        return 'key_rejected'
    if NETWORK.search(stderr):
        return 'network_unreachable'
    return 'provider_failed'


def turn(model, run=subprocess.run):
    """One attempt: '' and the reply when the usage record shows a completed turn, else the class."""
    with tempfile.TemporaryDirectory() as tmp:
        usage_file = Path(tmp) / 'usage.json'
        try:
            # From the agent's workspace, as a chat starts: the job itself runs in Hermes's source.
            result = run(['timeout', str(TIMEOUT), HERMES, '-z', PROMPT, '--usage-file', str(usage_file)],
                         capture_output=True, text=True, timeout=TIMEOUT + 30, cwd=WORKSPACE)
        except subprocess.TimeoutExpired:
            return 'timeout', ''
        if result.returncode != 0:
            return classify(result.returncode, result.stderr or ''), ''
        try:
            usage = json.loads(usage_file.read_text())
            ok = (usage.get('model') == model and int(usage.get('api_calls', 0)) >= 1
                  and int(usage.get('output_tokens', 0)) > 0 and usage.get('completed') is True)
        except (OSError, ValueError, TypeError, AttributeError):
            ok = False
        if not ok:
            return 'usage_record_invalid', ''
        return '', (result.stdout or '').strip()[:REPLY_LIMIT]


def main(argv, run=subprocess.run):
    if argv:
        print('usage: selftest', file=sys.stderr)
        return 2
    try:
        model = configured_model()
    except Exception:
        model = None
    if not model:
        print(json.dumps({'status': 'failed', 'class': 'config_unreadable'}))
        return 1
    failure = ''
    for _ in range(2):
        failure, reply = turn(model, run)
        if not failure:
            print(json.dumps({'status': 'passed', 'reply': reply}))
            return 0
    print(json.dumps({'status': 'failed', 'class': failure}))
    return 1
