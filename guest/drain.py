"""drain engage|cancel|status

Hermes's own external drain (gateway.drain_control): engage writes the marker that makes the
gateway refuse new chat turns and new cron runs while in-flight work finishes; cancel clears
it; status only reads. Every action prints the gateway's state as one JSON line. The status
file is written inside the agent's VM, so the host treats every field as untrusted.
"""
from datetime import datetime
import json
import sys


def instant(value):
    try:
        return datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    except ValueError:
        return None


def state():
    from gateway.drain_control import read_drain_request
    from gateway.status import get_running_pid, read_runtime_status

    runtime = read_runtime_status() or {}
    marker = read_drain_request() or {}
    requested, updated = instant(marker.get("requested_at")), instant(runtime.get("updated_at"))
    work = runtime.get("active_work") or []
    return {
        "running": get_running_pid() is not None,
        "marker": bool(marker),
        "state": runtime.get("gateway_state"),
        "active": runtime.get("active_agents"),
        # The status must postdate the marker, or "draining" could be an older drain's.
        "fresh": bool(requested and updated and updated >= requested),
        "work": [w.get("kind", "?") + (" " + str(round(w["elapsed_s"] / 60)) + "m" if w.get("elapsed_s") else "")
                 for w in work if isinstance(w, dict)],
    }


def main(argv):
    if len(argv) != 1 or argv[0] not in ("engage", "cancel", "status"):
        print("usage: drain engage|cancel|status", file=sys.stderr)
        return 2
    from gateway.drain_control import clear_drain_request, write_drain_request

    if argv[0] == "engage":
        write_drain_request(principal="operator-drain")
    elif argv[0] == "cancel":
        clear_drain_request()
    print(json.dumps(state()))
    return 0
