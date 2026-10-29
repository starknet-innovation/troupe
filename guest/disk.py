"""disk

How much the root filesystem holds: the state and workspace a resize archives live on it, so a
resize checks the host has room for twice that. The answer is the VM's, so the host still caps
each archive while it streams; this only spares a resize that could never fit.
"""
import json
import os
import sys


def used():
    stat = os.statvfs("/")
    return (stat.f_blocks - stat.f_bfree) * stat.f_frsize


def main(argv):
    if argv:
        print("usage: disk", file=sys.stderr)
        return 2
    print(json.dumps({"step": "disk", "status": "passed", "used_bytes": used()}))
    return 0
