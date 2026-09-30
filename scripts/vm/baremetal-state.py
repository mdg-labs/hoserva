#!/usr/bin/env python3
"""Helpers for the L3 suite's bare-metal-restore step (doc 06 §4, doc 10 §1).

  state <dir>               print the comparable state recorded in <dir>
  mapping <preview> <pool>  print the disk mapping to confirm, after checking
                            every array disk the archive records is matched
  report <report>           check a restore report lost no array disk

state reads shares.json, users.json, schedules.json, pool.json (the bodies of
listShares, listUsers, getSchedules and getPool) and mounts.txt (findmnt's
TARGET FSTYPE SOURCE lines) and prints only what a correct restore must
reproduce: next-run times, last-login times, usage figures and free space
change with time or with the restore itself and are left out, and so are
mounts that are not the array's (the pool disks' own mountpoints and
/mnt/user). Any missing field is an error, never an empty value.
"""
import json
import os
import sys

ARRAY_ROLES = ("data", "parity", "cache")
POOL_FSTYPES = ("ext4", "xfs", "fuse.mergerfs")


def load(path):
    with open(path) as f:
        return json.load(f)


def pick(obj, keys):
    return {k: obj[k] for k in keys}


def state(d):
    shares = sorted(
        (pick(s, ("name", "path", "cacheMode", "createPolicy", "smb", "nfs"))
         for s in load(os.path.join(d, "shares.json"))["shares"]),
        key=lambda s: s["name"],
    )
    users = sorted(
        (pick(u, ("id", "username", "role", "hasCredential"))
         for u in load(os.path.join(d, "users.json"))["users"]),
        key=lambda u: u["username"],
    )
    sched = load(os.path.join(d, "schedules.json"))
    chain = sched["chain"]
    schedules = {
        "chain": {
            "startTime": chain["startTime"],
            "weeklyScrubDay": chain["weeklyScrubDay"],
            "steps": [pick(s, ("id", "enabled")) for s in chain["steps"]],
        },
        "otherJobs": sorted(
            (pick(j, ("id", "enabled", "frequency", "time")) for j in sched["otherJobs"]),
            key=lambda j: j["id"],
        ),
        "conflicts": sorted(
            (pick(c, ("jobA", "jobB")) for c in sched["conflicts"]),
            key=lambda c: (c["jobA"], c["jobB"]),
        ),
    }
    pool = sorted(
        (pick(p, ("role", "mountPoint")) for p in load(os.path.join(d, "pool.json"))["disks"]),
        key=lambda p: (p["role"], p["mountPoint"]),
    )
    disk_mountpoints = {p["mountPoint"] for p in pool}
    mounts = []
    with open(os.path.join(d, "mounts.txt")) as f:
        for line in f:
            fields = line.split()
            if not fields:
                continue
            target, fstype, source = fields[0], fields[1], fields[2]
            is_pool_path = target in disk_mountpoints or target == "/mnt/user" or target.startswith("/mnt/user/")
            if is_pool_path and fstype in POOL_FSTYPES:
                mounts.append({
                    "target": target,
                    "fstype": fstype,
                    "branches": source if fstype == "fuse.mergerfs" else "",
                })
    mounts.sort(key=lambda m: m["target"])
    print(json.dumps(
        {"shares": shares, "users": users, "schedules": schedules, "poolDisks": pool, "mounts": mounts},
        indent=1, sort_keys=True,
    ))


def mapping(preview_path, pool_path):
    bare = load(preview_path)["bareMetal"]
    expected = sum(1 for p in load(pool_path)["disks"] if p["role"] in ARRAY_ROLES)
    disks = bare["disks"]
    if expected == 0:
        raise ValueError("the live pool lists no array disk, so there is nothing to restore onto")
    if len(disks) != expected:
        raise ValueError("the archive records %d array disks, the live pool had %d" % (len(disks), expected))
    unmatched = ["%s: %s" % (d["name"], d["state"]) for d in disks if d["state"] != "matched"]
    if unmatched:
        raise ValueError("array disks not matched against the attached disks: " + "; ".join(unmatched))
    entries = bare["diskMapping"]["disks"]
    if len(entries) != expected:
        raise ValueError("the mapping to confirm has %d entries for %d array disks" % (len(entries), expected))
    print(json.dumps(bare["diskMapping"]))


def report(path):
    rep = load(path)
    if not rep["restored"]:
        raise ValueError("the restore report lists nothing restored")
    lost = [n for n in rep["notRestored"] if n["kind"] == "disk"]
    for n in rep["notRestored"]:
        print("notRestored: %s %s (%s)" % (n["kind"], n["name"], n["reason"]))
    if lost:
        raise ValueError("the restore did not restore array disks: " + "; ".join(n["name"] for n in lost))


def main(argv):
    try:
        if len(argv) == 3 and argv[1] == "state":
            state(argv[2])
        elif len(argv) == 4 and argv[1] == "mapping":
            mapping(argv[2], argv[3])
        elif len(argv) == 3 and argv[1] == "report":
            report(argv[2])
        else:
            print(__doc__, file=sys.stderr)
            return 2
    except (OSError, ValueError, KeyError, IndexError, TypeError) as e:
        print("baremetal-state: %s: %s" % (type(e).__name__, e), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
