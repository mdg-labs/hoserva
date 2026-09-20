#!/usr/bin/env python3
"""Guest helper for the L3 soak (issue #44, doc 06 §6). Talks to hoservad
over the Unix socket as root (Q44). Stdlib only."""
from __future__ import annotations

import argparse
import http.client
import json
import os
import socket
import sqlite3
import sys
import time
import urllib.parse

SOCK = "/run/hoserva/hoserva.sock"
DB = "/var/lib/hoserva/hoserva.db"
KEEP_DIR = "/mnt/user/soak/keep"
CHURN_DIR = "/mnt/user/soak/churn"
KEEP_COUNT = 400
CHURN_COUNT = 1200
FILE_BYTES = 1024


class UnixHTTPConnection(http.client.HTTPConnection):
    def connect(self) -> None:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(self.timeout)
        sock.connect(SOCK)
        self.sock = sock


def api(method: str, path: str, body=None, timeout: float = 120):
    conn = UnixHTTPConnection("localhost", timeout=timeout)
    headers = {"Host": "unix"}
    payload = None
    if body is not None:
        payload = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    if not path.startswith("/"):
        path = "/" + path
    conn.request(method, "/api/v1" + path, body=payload, headers=headers)
    resp = conn.getresponse()
    raw = resp.read()
    conn.close()
    if not raw:
        return resp.status, None
    try:
        return resp.status, json.loads(raw.decode())
    except json.JSONDecodeError:
        return resp.status, raw.decode("utf-8", "replace")


def die(msg: str, status=None, body=None) -> None:
    extra = ""
    if status is not None:
        extra = f" (HTTP {status})"
    if body is not None:
        extra += f": {body!r}"
    print(f"soak-guest: {msg}{extra}", file=sys.stderr)
    sys.exit(1)


def cmd_api(args: argparse.Namespace) -> None:
    body = None
    if args.body:
        body = json.loads(args.body)
    status, parsed = api(args.method, args.path, body, timeout=args.timeout)
    print(json.dumps({"status": status, "body": parsed}, default=str))
    if status >= 400:
        sys.exit(1)


def cmd_wait_job(args: argparse.Namespace) -> None:
    terminal = {"succeeded", "failed", "cancelled", "interrupted"}
    deadline = time.monotonic() + args.timeout
    last = None
    while time.monotonic() < deadline:
        status, body = api("GET", f"/jobs/{args.job_id}", timeout=30)
        if status != 200 or not isinstance(body, dict):
            time.sleep(1)
            continue
        last = body
        st = body.get("status")
        if st in terminal:
            print(json.dumps(body, default=str))
            if st != "succeeded" and not args.allow_failure:
                sys.exit(2)
            return
        time.sleep(1)
    print(json.dumps(last, default=str))
    die(f"job {args.job_id} did not finish within {args.timeout}s")


def cmd_wait_idle(args: argparse.Namespace) -> None:
    deadline = time.monotonic() + args.timeout
    last_error = None
    saw_jobs = False
    while time.monotonic() < deadline:
        busy = False
        failed = False
        for st in ("queued", "running"):
            status, body = api("GET", f"/jobs?status={st}&limit=50", timeout=30)
            if status != 200 or not isinstance(body, dict):
                last_error = (status, body)
                failed = True
                break
            if body.get("jobs"):
                busy = True
                saw_jobs = True
                break
        if failed:
            time.sleep(1)
            continue
        if not busy:
            print(json.dumps({"idle": True}))
            return
        time.sleep(1)
    if last_error is not None and not saw_jobs:
        die(f"jobs query failed while waiting for idle after {args.timeout}s", last_error[0], last_error[1])
    die(f"jobs still running after {args.timeout}s")


def last_run_at() -> str:
    uri = "file:" + urllib.parse.quote(DB, safe="/") + "?mode=ro"
    con = sqlite3.connect(uri, uri=True, timeout=5)
    try:
        row = con.execute("select last_run_at from schedule_chain where id = 1").fetchone()
    finally:
        con.close()
    if not row or row[0] is None:
        return ""
    return str(row[0])


def cmd_last_run(_args: argparse.Namespace) -> None:
    print(last_run_at())


def cmd_wait_chain(args: argparse.Namespace) -> None:
    before = args.before
    deadline = time.monotonic() + args.timeout
    while time.monotonic() < deadline:
        now = last_run_at()
        if now and now != before:
            print(now)
            return
        time.sleep(1)
    die(f"maintenance chain last_run_at did not change within {args.timeout}s (still {before!r})")


def ensure_dirs() -> None:
    os.makedirs(KEEP_DIR, exist_ok=True)
    os.makedirs(CHURN_DIR, exist_ok=True)


def write_file(path: str, payload: bytes) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as fh:
        fh.write(payload)


def cmd_seed(_args: argparse.Namespace) -> None:
    ensure_dirs()
    for i in range(KEEP_COUNT):
        write_file(os.path.join(KEEP_DIR, f"{i:04d}.bin"), f"keep-{i}\n".encode().ljust(FILE_BYTES, b"\0"))
    for i in range(CHURN_COUNT):
        write_file(os.path.join(CHURN_DIR, f"{i:04d}.bin"), f"churn-{i}-v0\n".encode().ljust(FILE_BYTES, b"\0"))
    # Directly on disk1 so filling/unfilling that disk cannot leave it at
    # zero files (the zero-files guard is not Q16 and must not fire as a
    # side-effect of the full-disk injection).
    if os.path.ismount("/mnt/disk1"):
        write_file("/mnt/disk1/soak-disk1-keep.bin", b"disk1-keep\n".ljust(FILE_BYTES, b"\0"))
    print(json.dumps({"keep": KEEP_COUNT, "churn": CHURN_COUNT}))


def cmd_churn(args: argparse.Namespace) -> None:
    ensure_dirs()
    night = args.night
    added = 20
    edited = 15
    renamed = 10
    deleted = 8
    if args.mass_delete:
        deleted = 600
        added = 5
        edited = 5
        renamed = 5
    if args.heavy:
        added = 40
        write_file(os.path.join(CHURN_DIR, f"heavy-{night}.bin"), os.urandom(256 * 1024 * 1024))

    created = []
    for i in range(added):
        name = f"n{night:02d}-add-{i:03d}.bin"
        path = os.path.join(CHURN_DIR, name)
        write_file(path, f"add-n{night}-{i}\n".encode().ljust(FILE_BYTES, b"\0"))
        created.append(name)

    edited_names = []
    for i in range(edited):
        idx = (night * 17 + i) % CHURN_COUNT
        name = f"{idx:04d}.bin"
        path = os.path.join(CHURN_DIR, name)
        if os.path.exists(path):
            write_file(path, f"churn-{idx}-v{night}\n".encode().ljust(FILE_BYTES, b"\0"))
            edited_names.append(name)

    renamed_pairs = []
    for i in range(renamed):
        idx = (night * 29 + i + 3) % CHURN_COUNT
        src = os.path.join(CHURN_DIR, f"{idx:04d}.bin")
        dst_name = f"{idx:04d}-ren-n{night:02d}.bin"
        dst = os.path.join(CHURN_DIR, dst_name)
        if os.path.exists(src) and not os.path.exists(dst):
            os.rename(src, dst)
            renamed_pairs.append([f"{idx:04d}.bin", dst_name])

    deleted_names = []
    names = sorted(n for n in os.listdir(CHURN_DIR) if n.endswith(".bin") and not n.startswith("heavy-"))
    for name in names[:deleted]:
        path = os.path.join(CHURN_DIR, name)
        os.remove(path)
        deleted_names.append(name)

    print(json.dumps({
        "night": night,
        "added": len(created),
        "edited": len(edited_names),
        "renamed": len(renamed_pairs),
        "deleted": len(deleted_names),
        "massDelete": bool(args.mass_delete),
        "heavy": bool(args.heavy),
    }))


def data_mounts() -> list:
    mounts = []
    for i in range(1, 8):
        path = f"/mnt/disk{i}"
        if os.path.ismount(path):
            mounts.append(path)
    return mounts


def cmd_checksum(args: argparse.Namespace) -> None:
    import hashlib

    rows = []
    roots = args.roots or data_mounts()
    for root in roots:
        for dirpath, dirnames, filenames in os.walk(root):
            dirnames[:] = [d for d in dirnames if d != "lost+found"]
            for name in filenames:
                if name == "soak-fill.bin" or name.startswith("snapraid.content"):
                    continue
                path = os.path.join(dirpath, name)
                rel = os.path.relpath(path, "/")
                h = hashlib.sha256()
                with open(path, "rb") as fh:
                    for chunk in iter(lambda: fh.read(1024 * 1024), b""):
                        h.update(chunk)
                rows.append(f"{h.hexdigest()}  {rel}")
    rows.sort()
    text = "\n".join(rows) + ("\n" if rows else "")
    args.output.write(text)
    print(json.dumps({"files": len(rows), "path": args.output.name}))


def cmd_fill(_args: argparse.Namespace) -> None:
    target = "/mnt/disk1/soak-fill.bin"
    st = os.statvfs("/mnt/disk1")
    free = st.f_bavail * st.f_frsize
    size = max(0, free - 8 * 1024 * 1024)
    if size < 64 * 1024 * 1024:
        die(f"/mnt/disk1 only has {free} free bytes; cannot fill")
    fd = os.open(target, os.O_CREAT | os.O_RDWR, 0o644)
    try:
        os.posix_fallocate(fd, 0, size)
        os.lseek(fd, 0, os.SEEK_SET)
        os.write(fd, b"SOAK-FILL")
    finally:
        os.close(fd)
    st = os.statvfs("/mnt/disk1")
    print(json.dumps({
        "path": target,
        "bytes": size,
        "freeBytes": st.f_bavail * st.f_frsize,
        "totalBytes": st.f_blocks * st.f_frsize,
    }))


def cmd_unfill(_args: argparse.Namespace) -> None:
    path = "/mnt/disk1/soak-fill.bin"
    if os.path.exists(path):
        os.remove(path)
    print(json.dumps({"removed": path}))


def cmd_create_array(args: argparse.Namespace) -> None:
    status, body = api("GET", "/disks", timeout=60)
    if status != 200 or not isinstance(body, dict):
        die("list disks failed", status, body)
    parity, data, cache = [], [], []
    for d in body.get("disks", []):
        if d.get("boot"):
            continue
        serial = d.get("serial") or ""
        device = d.get("device")
        if not device:
            continue
        assignment = {"device": device, "role": "data", "filesystem": "xfs"}
        if serial.startswith("parity"):
            assignment["role"] = "parity"
            parity.append(assignment)
        elif serial.startswith("cache"):
            assignment["role"] = "cache"
            cache.append(assignment)
        elif serial.startswith("disk"):
            data.append(assignment)
    if not parity or not data:
        die(f"could not classify array disks: parity={parity!r} data={data!r}")
    disks = parity + data + cache
    erase = sorted(a["device"] for a in disks)
    confirmation = "ERASE " + ", ".join(erase)
    req = {
        "disks": disks,
        "confirmation": confirmation,
        "minFreeSpace": args.min_free_space,
    }
    status, body = api("POST", "/disks/array", req, timeout=60)
    if status != 200:
        die("create array failed", status, body)
    print(json.dumps(body, default=str))


def cmd_diff(_args: argparse.Namespace) -> None:
    status, body = api("POST", "/parity/diff", timeout=600)
    if status != 200:
        die("parity diff failed", status, body)
    print(json.dumps(body, default=str))


def cmd_notifications(_args: argparse.Namespace) -> None:
    status, body = api("GET", "/notifications", timeout=30)
    if status != 200:
        die("list notifications failed", status, body)
    print(json.dumps(body, default=str))


def cmd_running_jobs(_args: argparse.Namespace) -> None:
    status, body = api("GET", "/jobs?status=running&limit=50", timeout=30)
    if status != 200:
        die("list running jobs failed", status, body)
    print(json.dumps(body, default=str))


def cmd_sync(args: argparse.Namespace) -> None:
    status, body = api("POST", "/parity/sync", {"dryRun": False, "confirm": bool(args.confirm)}, timeout=60)
    if status != 200:
        die("start sync failed", status, body)
    print(json.dumps(body, default=str))


def cmd_setup_admin(args: argparse.Namespace) -> None:
    status, body = api("POST", "/setup/admin", {
        "username": args.username,
        "password": args.password,
    }, timeout=60)
    if status != 200:
        die("create first admin failed", status, body)
    print(json.dumps({"username": (body or {}).get("username") if isinstance(body, dict) else None}, default=str))


def cmd_array_start(_args: argparse.Namespace) -> None:
    status, body = api("POST", "/array/start", timeout=120)
    if status != 200:
        die("array start failed", status, body)
    print(json.dumps(body, default=str))


def cmd_jobs(_args: argparse.Namespace) -> None:
    status, body = api("GET", "/jobs?limit=20", timeout=30)
    if status != 200:
        die("list jobs failed", status, body)
    print(json.dumps(body, default=str))


def main() -> None:
    p = argparse.ArgumentParser()
    sub = p.add_subparsers(dest="cmd", required=True)

    ap = sub.add_parser("api")
    ap.add_argument("method")
    ap.add_argument("path")
    ap.add_argument("--body", default="")
    ap.add_argument("--timeout", type=float, default=120)
    ap.set_defaults(func=cmd_api)

    wj = sub.add_parser("wait-job")
    wj.add_argument("job_id")
    wj.add_argument("--timeout", type=float, default=1800)
    wj.add_argument("--allow-failure", action="store_true")
    wj.set_defaults(func=cmd_wait_job)

    wi = sub.add_parser("wait-idle")
    wi.add_argument("--timeout", type=float, default=1800)
    wi.set_defaults(func=cmd_wait_idle)

    sub.add_parser("last-run").set_defaults(func=cmd_last_run)

    wc = sub.add_parser("wait-chain")
    wc.add_argument("--before", default="")
    wc.add_argument("--timeout", type=float, default=120)
    wc.set_defaults(func=cmd_wait_chain)

    sub.add_parser("seed").set_defaults(func=cmd_seed)

    ch = sub.add_parser("churn")
    ch.add_argument("night", type=int)
    ch.add_argument("--mass-delete", action="store_true")
    ch.add_argument("--heavy", action="store_true")
    ch.set_defaults(func=cmd_churn)

    cs = sub.add_parser("checksum")
    cs.add_argument("-o", "--output", type=argparse.FileType("w"), required=True)
    cs.add_argument("roots", nargs="*")
    cs.set_defaults(func=cmd_checksum)

    sub.add_parser("fill").set_defaults(func=cmd_fill)
    sub.add_parser("unfill").set_defaults(func=cmd_unfill)

    ca = sub.add_parser("create-array")
    ca.add_argument("--min-free-space", default="32M")
    ca.set_defaults(func=cmd_create_array)

    sub.add_parser("diff").set_defaults(func=cmd_diff)
    sub.add_parser("notifications").set_defaults(func=cmd_notifications)
    sub.add_parser("running-jobs").set_defaults(func=cmd_running_jobs)
    sy = sub.add_parser("sync")
    sy.add_argument("--confirm", action="store_true")
    sy.set_defaults(func=cmd_sync)
    sa = sub.add_parser("setup-admin")
    sa.add_argument("--username", required=True)
    sa.add_argument("--password", required=True)
    sa.set_defaults(func=cmd_setup_admin)
    sub.add_parser("array-start").set_defaults(func=cmd_array_start)
    sub.add_parser("jobs").set_defaults(func=cmd_jobs)

    args = p.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
