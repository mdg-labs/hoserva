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
  flash/                 files laid over common/flash/, including config/hoserva/,
                         the committed Phase A capture
```

Build and check a variant with `make lab-unraid-fixture` and `make lab-unraid-verify`
(L2, loop devices) or `make vm-unraid-fixture` (L3, the lab's guest); regenerate a
variant's capture with `make vm-unraid-capture`. All three take `VARIANT=<variant>`.
Doc 06 §5 describes what is built and what is recorded.
