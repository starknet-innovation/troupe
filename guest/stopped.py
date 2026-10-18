"""gateway-stopped

No gateway runs in the VM. Prints {"status": "passed"} or {"status": "failed", "class":
"gateway_running"}.
"""
import json
import sys


def main(argv):
    if argv:
        print("usage: gateway-stopped", file=sys.stderr)
        return 2
    from gateway.status import get_running_pid

    if get_running_pid() is not None:
        print(json.dumps({"status": "failed", "class": "gateway_running"}))
        return 1
    print(json.dumps({"status": "passed"}))
    return 0
