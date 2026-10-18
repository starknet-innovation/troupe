"""storage ROOT_GIB DOCKER_GIB HOST_WORKSPACE|-

The VM's disks and mounts match its declared storage exactly: the root and Docker disk
sizes, a Docker mount only when declared, no host folder but the declared workspace, and no
SSH agent socket forwarded from the host.
"""
import json
import os
from pathlib import Path
import subprocess
import sys

GIB = 1024 ** 3


def check(root_gib, docker_gib, host_workspace=''):
    devices = json.loads(subprocess.check_output(
        ["lsblk", "-b", "-d", "-J", "-o", "NAME,SIZE,RO,TYPE"], text=True))["blockdevices"]
    writable = sorted(int(d["size"]) for d in devices
                      if d["type"] == "disk" and not d["ro"] and int(d["size"]) > 0)
    expected = sorted([root_gib * GIB] + ([docker_gib * GIB] if docker_gib else []))
    assert writable == expected, "step=storage class=device_capacity_mismatch"
    root_stat = os.statvfs("/")
    root_capacity = root_stat.f_frsize * root_stat.f_blocks
    assert root_gib * GIB * 0.9 < root_capacity <= root_gib * GIB, "step=storage class=root_filesystem_size_mismatch"
    if docker_gib:
        assert os.path.ismount("/var/lib/docker"), "step=storage class=docker_mount_missing"
        docker_stat = os.statvfs("/var/lib/docker")
        assert docker_gib * GIB * 0.9 < docker_stat.f_frsize * docker_stat.f_blocks <= docker_gib * GIB, \
            "step=storage class=docker_filesystem_size_mismatch"
    else:
        assert not Path("/var/run/docker.sock").exists(), "step=storage class=unexpected_docker_socket"
        assert not os.path.ismount("/var/lib/docker"), "step=storage class=unexpected_docker_mount"
    host_directories = []
    for line in Path("/proc/self/mountinfo").read_text().splitlines():
        left, right = line.split(" - ", 1)
        target = left.split()[4].replace("\\040", " ")
        if right.split()[0] == "virtiofs" and Path(target).is_dir():
            host_directories.append(target)
    assert host_directories == ([host_workspace] if host_workspace else []), \
        "step=storage class=unexpected_host_directory_mount"
    assert Path(host_workspace or "/home/agent/workspace").is_dir(), "step=storage class=workspace_missing"
    ssh_socket = os.environ.get("SSH_AUTH_SOCK")
    assert not (ssh_socket and Path(ssh_socket).exists()), "step=storage class=ssh_agent_socket_present"
    return {"step": "storage", "status": "passed", "root_gib": root_gib,
            "docker_gib": docker_gib, "host_workspaces": host_directories}


def main(argv):
    if len(argv) != 3:
        print("usage: storage ROOT_GIB DOCKER_GIB HOST_WORKSPACE|-", file=sys.stderr)
        return 2
    try:
        result = check(int(argv[0]), int(argv[1]), '' if argv[2] == '-' else argv[2])
    except AssertionError as error:
        # The message is `step=storage class=NAME`; report only the class.
        print(json.dumps({"step": "storage", "status": "failed", "class": str(error).rpartition("class=")[2]}))
        return 1
    print(json.dumps(result))
    return 0
