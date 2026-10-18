"""ready [--platform NAME]

The gateway is running: its PID is alive and its runtime status says "running". With
--platform, that platform is the only one the gateway's config connects, and the running
gateway reports it connected and not needing attention.
Prints {"status": "passed"} or {"status": "failed", "class": ...}.
"""
import json
import os
import sys


def check(platform):
    from gateway.status import get_running_pid, read_runtime_status

    pid = get_running_pid()
    if not pid or not os.path.isdir(f"/proc/{pid}"):
        return "gateway_not_running"
    if (read_runtime_status() or {}).get("gateway_state") != "running":
        return "gateway_not_ready"
    if platform:
        from gateway.config import load_gateway_config

        connected = [getattr(p, "value", str(p)) for p in load_gateway_config().get_connected_platforms()]
        if connected != [platform]:
            return "platform_not_connected"
        # The live connection, as this gateway process reports it, not only the config.
        runtime = read_runtime_status() or {}
        state = (runtime.get("platforms") or {}).get(platform) or {}
        if runtime.get("pid") != pid or state.get("state") != "connected" or state.get("needs_attention"):
            return "platform_not_connected"
    return ""


def main(argv):
    if argv and (len(argv) != 2 or argv[0] != "--platform" or not argv[1]):
        print("usage: ready [--platform NAME]", file=sys.stderr)
        return 2
    failure = check(argv[1] if argv else "")
    if failure:
        print(json.dumps({"status": "failed", "class": failure}))
        return 1
    print(json.dumps({"status": "passed"}))
    return 0
