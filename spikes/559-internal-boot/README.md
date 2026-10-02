# Spike 559 — Unraid 7.3 internal boot, device layout

Findings are in `docs/internal/08-spike-findings.md` (Spike 2, "Unraid 7.3 internal boot — device layout and what the migrator sees"). This directory holds the experiment scripts and the raw output of the lab run behind its lab claims.

## What it checks

The layout itself is read from Unraid's public source (`unraid/webgui` `ungrub/mkbootable`), not measured. The lab part checks what libblkid and `partx` report for that layout and whether the partition-name rule Unraid's own API uses separates it from other disks:

- `scripts/build-image.py` (host side, writes regular files only, runs no storage tool) writes sparse images: a dedicated boot device, a boot + data device, a two-device mirror pair, a GPT array disk (negative control) and an internal-boot image with partition 3 renamed (negative control). Partition 3 carries a synthetic ZFS vdev label (pool `flash`, state exported) carrying the keys libblkid reads; it is not a pool. Partition 2 has no filesystem (no `mkfs.fat` in the lab image).
- `scripts/probe.sh` (in the lab) reads each partition table with `blkid -p` and `partx`, then probes every partition through its own offset loop device with `blkid -p -o udev`. `--format-data btrfs|xfs` formats partition 4 first.
- `scripts/classify.sh` (in the lab) applies the fingerprint rule to every image and exits non-zero if a verdict differs from what the image's name promises.
- `scripts/run-all.sh` (host) builds the images and runs the three steps against your own lab.

## Run

```
make lab-up HOSERVA_LAB_ID=<id>
HOSERVA_LAB_ID=<id> bash spikes/559-internal-boot/scripts/run-all.sh
make lab-destroy HOSERVA_LAB_ID=<id>
```

`results/` is the output of the run the findings quote: `build.log`, `probe-baseline.log`, `probe-data-btrfs.log`, `probe-data-xfs.log`, `classify.log`, `classify-after-format.log` and `versions.log` (util-linux 2.41.5, btrfs-progs 6.14, xfsprogs 6.13). GUIDs differ on every run.

## Limits

The loop devices are major 7 only (the lab's device cgroup), so partition device nodes (major 259) are never opened; partitions are reached with `losetup --offset/--sizelimit`. That is why `ID_PART_ENTRY_*` is read from the partition table (`partx`) and not from a `blkid -o udev` of a partition node. The classifier here reads an image file with `partx`; it is not the Go code that will do the same from the udev database.
