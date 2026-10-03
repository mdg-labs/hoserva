# Synthetic Unraid sources

Definitions of the Unraid arrays the migration tests start from, built without
Unraid by `scripts/devenv/unraid-fixture.sh` (doc 06 §5). Every file here is
authored for Hoserva; nothing is copied from a real server or from a third-party
template catalog.

```
common/                  shared by every variant
  flash/                 the flash tree: config/, syslinux/, the templates, a Compose
                         Manager project, a User Scripts entry
  seed-*                 seed data, one operation per line (see seed-array)
  spec-flash             flash extras every variant shares
  runtime/               Unraid runtime state the capture run reads (var.ini, autostart, smart)
<variant>/
  spec                   disks, filesystems, sizes, Unraid version
  seed                   which seed files apply, plus variant-only data
  expect                 only on the refusal and filesystem variants: the verdict the
                         scan must give each disk (see below)
  flash/                 files laid over common/flash/, including config/hoserva/,
                         the committed Phase A capture
  options/<name>/        only where a variant has options (unraid-with-vms): the
                         option's seed and a flash/ laid over the variant's
  libvirt/               only unraid-with-vms: the tree of its libvirt.img, one
                         qemu/<name>.xml per domain
```

Build and check a variant with `make lab-unraid-fixture` and `make lab-unraid-verify`
(L2, loop devices) or `make vm-unraid-fixture` (L3, the lab's guest); regenerate a
variant's capture with `make vm-unraid-capture`. All three take `VARIANT=<variant>`,
and `OPTION=<name>` for a variant with options (`unraid-with-vms`: `array`, the
default, `alt` and `cache`).
Doc 06 §5 describes what is built and what is recorded.

Variants for the scan's per-filesystem checks and refusals:

| Variant | What it holds | Built on |
|---|---|---|
| `unraid-btrfs-and-ext4-disks` | an ext4 and a single-device btrfs disk beside two XFS ones, all adoptable; no `config/hoserva/` | L2, L3 |
| `unraid-corrupt-xfs` | one XFS disk whose metadata fails `xfs_repair -n`; refused, the rest proceed | L2, L3 |
| `unraid-encrypted` | LUKS containers holding XFS (`luks:xfs`); each refused | L3 only |
| `unraid-zfs-disk` | a ZFS array disk and a two-device btrfs filesystem; each refused | L3 only |
| `unraid-internal-boot` | Unraid 7.3 dedicated boot pool, no USB stick; the boot device is recognised, not refused | L3 only |
| `unraid-internal-boot-shared` | the same with a boot + data device whose data area is the cache | L3 only |

L3 only means the variant needs OpenZFS or device-mapper, which the lab image and
container lack: `make lab-unraid-fixture` refuses it and names
`make vm-unraid-fixture`. An `expect` file lists one verdict per disk; the builder
measures each on the disk and refuses a variant whose file disagrees, then writes
the checked result as `expected/scan.txt` (and, for a variant that refuses a disk,
a whole-device sha256 of every source disk as `expected/source-disks.sha256`).
Variants without a `config/hoserva/` say `capture=none` in their spec, and
`make vm-unraid-capture` refuses them.
