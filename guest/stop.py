"""stop-gateway

Stops the externally supervised gateway: writes Hermes's planned-stop marker, sends SIGTERM to
the HERMES_HOME-scoped gateway and waits up to 60 seconds without force-killing it, then
clears any drain marker so a later start accepts work. Prints {"status", "state"}.
"""
import json
import os
import signal
import sys
import time

WAIT_SECONDS = 60


def clear_drain():
    # A drain ends with the stop. Best effort, so the stop never depends on the drain module.
    try:
        from gateway.drain_control import clear_drain_request
        clear_drain_request()
    except Exception:
        pass


def main(argv):
    if argv:
        print("usage: stop-gateway", file=sys.stderr)
        return 2
    from gateway.status import get_running_pid, write_planned_stop_marker

    pid = get_running_pid()
    if pid is None:
        clear_drain()
        print(json.dumps({"status": "passed", "state": "already_stopped"}))
        return 0
    write_planned_stop_marker(pid)
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    deadline = time.monotonic() + WAIT_SECONDS
    while time.monotonic() < deadline:
        if get_running_pid() is None:
            clear_drain()
            print(json.dumps({"status": "passed", "state": "stopped"}))
            return 0
        time.sleep(0.5)
    print(json.dumps({"status": "failed", "class": "gateway_still_running"}))
    return 1
