#!/usr/bin/env python3
"""Writes sparse image files laid out like an Unraid 7.3 internal boot device.

Pure file writes: no block device, loop device, mount or partitioning tool is
touched, so it runs on the dev host. It reproduces the GPT that
`ungrub/mkbootable` (unraid/webgui) makes with sgdisk, and a synthetic ZFS vdev
label in partition 3 (pool name `flash`, state exported) that libblkid's
`zfs_member` probe accepts. The label carries only the keys libblkid reads; no
pool made from it can be imported. Partition 2 is left without a filesystem
(mkbootable formats it FAT32 labelled EFI; the lab image has no mkfs.fat).

  build-image.py --out DIR [--mode shared|dedicated|array-gpt] [--devices 1|2]
                 [--boot-size-mib 1024] [--disk-mib 2048] [--p3-name NAME]

shared: partition 4 is the data area, from `boot-size-mib` to the end of the
disk. dedicated: `boot-size-mib` is rewritten to disk size minus 1 MiB, the way
create_internal_boot_user.sh does for `--size 0`, so partition 4 is under 1 MiB.
array-gpt: a negative control, one partition from sector 64 to 33 sectors before
the end of the disk, type 0fc63daf, as a real 7.3.2 array disk over 2 TB has
(doc 08 section 2). --p3-name renames partition 3, a second negative control.
"""
import argparse
import os
import random
import struct
import uuid
import zlib

SECTOR = 512

BIOS_BOOT = uuid.UUID("21686148-6449-6e6f-744e-656564454649")
ESP = uuid.UUID("c12a7328-f81f-11d2-ba4b-00a0c93ec93b")
LINUX = uuid.UUID("0fc63daf-8483-4772-8e79-3d69d8477de4")

DATA_TYPE_UINT64 = 8
DATA_TYPE_STRING = 9
DATA_TYPE_DIRECTORY = 19


def pad4(n):
    return (n + 3) & ~3


def nv_name(name):
    raw = name.encode()
    return struct.pack(">I", len(raw)) + raw + b"\0" * (pad4(len(raw)) - len(raw))


def nv_pair(name, value_bytes):
    body = nv_name(name) + value_bytes
    size = 8 + len(body)
    return struct.pack(">II", size, size) + body


def nv_u64(name, v):
    return nv_pair(name, struct.pack(">IIQ", DATA_TYPE_UINT64, 1, v))


def nv_str(name, s):
    raw = s.encode()
    return nv_pair(
        name,
        struct.pack(">III", DATA_TYPE_STRING, 1, len(raw)) + raw + b"\0" * (pad4(len(raw)) - len(raw)),
    )


def nv_dir(name, inner):
    head = struct.pack(">IIII", DATA_TYPE_DIRECTORY, 1, 0, 1)
    return nv_pair(name, head) + inner + struct.pack(">II", 0, 0)


def zfs_label(pool_name, pool_guid, vdev_guid, state):
    tree = nv_str("type", "disk") + nv_u64("id", 0) + nv_u64("guid", vdev_guid) + nv_u64("ashift", 12)
    pairs = [
        nv_u64("version", 5000),
        nv_str("name", pool_name),
        nv_u64("state", state),
        nv_u64("txg", 4),
        nv_u64("pool_guid", pool_guid),
        nv_u64("guid", vdev_guid),
        nv_dir("vdev_tree", tree),
    ]
    return struct.pack(">BBBBII", 1, 1, 0, 0, 0, 1) + b"".join(pairs) + struct.pack(">II", 0, 0)


def label_offset(size, n):
    return n * 256 * 1024 + (0 if n < 2 else size - 4 * 256 * 1024 - (size % (256 * 1024)))


def gpt_entry(ptype, first, last, name):
    raw = name.encode("utf-16-le")[:72]
    return (
        ptype.bytes_le
        + uuid.uuid4().bytes_le
        + struct.pack("<QQQ", first, last, 0)
        + raw
        + b"\0" * (72 - len(raw))
    )


def gpt_header(cur, backup, first_usable, last_usable, disk_guid, entries_lba, entries_crc):
    h = (
        b"EFI PART"
        + struct.pack("<IIIIQQQQ", 0x00010000, 92, 0, 0, cur, backup, first_usable, last_usable)
        + disk_guid.bytes_le
        + struct.pack("<QIII", entries_lba, 128, 128, entries_crc)
    )
    crc = zlib.crc32(h) & 0xFFFFFFFF
    h = h[:16] + struct.pack("<I", crc) + h[20:]
    return h + b"\0" * (SECTOR - len(h))


def write_gpt(f, total, parts):
    last_usable = total - 34
    table = b"".join(parts) + b"\0" * (128 * 128 - 128 * len(parts))
    table_crc = zlib.crc32(table) & 0xFFFFFFFF
    disk_guid = uuid.uuid4()
    mbr = bytearray(SECTOR)
    mbr[446 + 1 : 446 + 4] = b"\x00\x02\x00"
    mbr[446 + 4] = 0xEE
    mbr[446 + 5 : 446 + 8] = b"\xff\xff\xff"
    struct.pack_into("<II", mbr, 446 + 8, 1, min(total - 1, 0xFFFFFFFF))
    mbr[510:512] = b"\x55\xaa"
    f.truncate(total * SECTOR)
    f.seek(0)
    f.write(mbr)
    f.write(gpt_header(1, total - 1, 34, last_usable, disk_guid, 2, table_crc))
    f.write(table)
    f.seek((total - 33) * SECTOR)
    f.write(table)
    f.write(gpt_header(total - 1, 1, 34, last_usable, disk_guid, total - 33, table_crc))


def build_array_gpt(path, disk_mib):
    total = disk_mib * 2048
    with open(path, "wb") as f:
        write_gpt(f, total, [gpt_entry(LINUX, 64, total - 34, "")])


def build_internal_boot(path, boot_mib, disk_mib, mode, pool_name, pool_guid, vdev_guid, p3_name):
    total = disk_mib * 2048
    if mode == "dedicated":
        boot_mib = disk_mib - 1
    p1 = (2048, 2048 + 2048 - 1)
    p2 = (4096, 4096 + 510 * 2048 - 1)
    p3 = (512 * 2048, boot_mib * 2048 - 1)
    p4 = (boot_mib * 2048, total - 34)
    parts = [
        gpt_entry(BIOS_BOOT, *p1, "BIOS Boot Partition"),
        gpt_entry(ESP, *p2, "EFI System Partition"),
        gpt_entry(LINUX, *p3, p3_name),
        gpt_entry(LINUX, *p4, ""),
    ]
    with open(path, "wb") as f:
        write_gpt(f, total, parts)
        p3_bytes = (p3[1] - p3[0] + 1) * SECTOR
        label = zfs_label(pool_name, pool_guid, vdev_guid, 1)
        for n in range(4):
            f.seek(p3[0] * SECTOR + label_offset(p3_bytes, n) + 16 * 1024)
            f.write(label)
    return {"p1": p1, "p2": p2, "p3": p3, "p4": p4}


def refuse_non_regular(path):
    if os.path.islink(path):
        raise SystemExit(f"refusing {path}: symbolic link")
    if os.path.lexists(path) and not os.path.isfile(path):
        raise SystemExit(f"refusing {path}: not a regular file")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--mode", choices=["shared", "dedicated", "array-gpt"], default="shared")
    ap.add_argument("--devices", type=int, choices=[1, 2], default=1)
    ap.add_argument("--boot-size-mib", type=int, default=1024)
    ap.add_argument("--disk-mib", type=int, default=2048)
    ap.add_argument("--pool-name", default="flash")
    ap.add_argument("--p3-name", default="Unraid Boot Partition")
    a = ap.parse_args()
    if a.boot_size_mib < 1024 or a.boot_size_mib >= a.disk_mib:
        ap.error("--boot-size-mib must be at least 1024 and below --disk-mib")
    os.makedirs(a.out, exist_ok=True)

    if a.mode == "array-gpt":
        path = os.path.join(a.out, "array-gpt-1.img")
        refuse_non_regular(path)
        build_array_gpt(path, a.disk_mib)
        print(os.path.basename(path))
        return

    rng = random.SystemRandom()
    pool_guid = rng.getrandbits(63) | (1 << 62)
    suffix = "" if a.p3_name == "Unraid Boot Partition" else "-renamed"
    for i in range(1, a.devices + 1):
        path = os.path.join(a.out, f"internal-boot-{a.mode}{suffix}-{i}.img")
        refuse_non_regular(path)
        vdev_guid = rng.getrandbits(63) | (1 << 62)
        info = build_internal_boot(path, a.boot_size_mib, a.disk_mib, a.mode, a.pool_name, pool_guid, vdev_guid, a.p3_name)
        print(f"{os.path.basename(path)} pool_guid={pool_guid} vdev_guid={vdev_guid}")
        for k in ("p1", "p2", "p3", "p4"):
            s, e = info[k]
            print(f"  {k}: start_sector={s} sectors={e - s + 1}")


if __name__ == "__main__":
    main()
