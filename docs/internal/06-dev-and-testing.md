# Hoserva — Development Workflow and Testing

## The problem

Hoserva partitions disks, mounts filesystems, writes to `/etc`, and runs as root. **It cannot be installed on a development machine.** Running it there would repartition the dev box's drives.

Worse, the interesting behaviour — a disk failing mid-sync, parity reconstruction, a mover run interrupted by power loss — cannot be triggered on real hardware without destroying real data or waiting hours.

The entire development approach has to be built around this constraint from day one. Retrofitting testability onto a tool that shells out to `mkfs` is not feasible.

---

## 1. The three-layer test pyramid

| Layer | Runs on | Speed | What it covers |
|---|---|---|---|
| **L1 — Unit** | Dev machine, any OS | seconds | Config generation, parsers, business logic, API handlers |
| **L2 — Loop-device integration** | Container or VM, Linux | seconds to minutes | Real mergerfs, real SnapRAID, real filesystems, on fake disks |
| **L3 — VM end-to-end** | QEMU/KVM under the user's `qemu:///session` | minutes to hours | Full install, virtual block devices, disk failure injection, UI flows, soak test, nested passthrough |

Nearly all development happens at L1 and L2. L3 runs nightly and before releases. **There is no hardware layer (D20):** every test is run by an agent, never on the maintainer's machines, and behaviour only physical disks show is covered by the proxies in §6 with its residual risk stated.

---

## 2. L1 — Unit tests and the fake providers

### The interfaces

Every subsystem touching the real system sits behind an interface (doc 01 §4). That yields two implementations each: a real one and a fake one.

```go
// internal/disk/provider.go
type Provider interface {
    List(ctx) ([]Disk, error)
    SMART(ctx, dev string) (SMARTReport, error)
    Spindown(ctx, dev string) error
    Format(ctx, dev string, fs FilesystemType) error
}

// internal/disk/fake.go — used by unit tests and the frontend dev server
type FakeProvider struct {
    disks map[string]*FakeDisk
}
```

The fake provider is not a stub. It is a **simulator** with scriptable behaviour:

```go
fake.AddDisk("/dev/sdb", Disk{Size: 8 * TB, Model: "WD80EFZX", Serial: "WCC4N1234567"})
fake.SetSMART("/dev/sdb", SMARTReport{ReallocatedSectors: 4, Trend: Rising})
fake.FailAfter("/dev/sdc", 30*time.Second)     // disk dies mid-operation
fake.SetSpinState("/dev/sdd", Standby)
fake.SlowDown("/dev/sde", 10*time.Millisecond) // simulate a dying-slow disk
```

This makes otherwise untestable scenarios cheap:

- Parity disk smaller than the largest data disk → setup must refuse
- A disk reporting rising reallocated sectors → alert must fire, migration scan must warn
- A disk disappearing mid-sync → job must fail cleanly, not corrupt state
- SMART polling must not wake a standby disk → assert the fake was never queried in a waking mode

### Golden-file config tests

Config generation is pure: state in, text out. Test it with golden files.

```
testdata/
  configs/
    6disk-1parity-cache/
      state.json           # the SQLite state as JSON
      snapraid.conf.golden
      smb.conf.golden
      pool.mount.golden
```

Any change to generated output shows up as a reviewable diff in the PR. This is the highest-value, lowest-cost testing in the project, because config generation is where correctness actually lives.

### Schema-migration upgrade tests (D16)

- `testdata/db/<schema-version>.db` holds one fixture database per released schema version, with representative rows in every table: shares, users, encrypted secrets, job history, an imported Unraid setup. It is created when a release is cut and never regenerated, like a golden file.
- CI upgrades every fixture to head through the real runner and asserts that every row and value survives, or equals its data transform's tested output.
- Replaying all migrations into an empty database must produce exactly `schema.sql`, and no existing migration may differ from its recorded checksum.
- A failure injected partway through an upgrade must leave the database unchanged (`PRAGMA integrity_check` clean, every row equal to the pre-migration snapshot) and the daemon stopped.

### Parser tests against real-world corpus

`snapraid diff`, `snapraid status`, `smartctl -j`, `docker` output, and Unraid XML all get parsed. Collect real outputs into `testdata/` and test against them — including malformed and edge cases. Unraid XML especially: the converter must handle every template in the corpus below without panicking, with a tracked count of how many convert cleanly vs. with warnings (clean as defined in Q36). **That number is a release metric.**

**The template corpus is written by the project.** `testdata/unraid-templates/` holds Unraid-format XML templates authored for testing — every field in doc 04 §5, common `ExtraParams` flags, path and network edge cases, and malformed input — never copied from a third-party catalog. `make test-corpus` converts all of them and reports the clean-conversion rate (Q36); CI fails if it regresses.

---

## 3. L2 — The loop-device harness

**This is the core of the development workflow.** It gives real mergerfs and real SnapRAID behaviour on a laptop, in seconds, with zero risk.

### How it works

Linux loop devices turn sparse files into block devices. A sparse 8 TB file consumes only the bytes actually written, so a full six-disk array with realistic capacity costs almost nothing on disk.

```bash
#!/usr/bin/env bash
# scripts/devenv/create-array.sh
set -euo pipefail
: "${HOSERVA_LAB_ID:?set a lab id — parallel labs must never share loop devices or paths}"
LAB=/lab/$HOSERVA_LAB_ID                         # inside the lab container; bind-mounted from the workspace
mkdir -p "$LAB"/{img,mnt} "$LAB/mnt/user"

create_disk() {  # name, size
  truncate -s "$2" "$LAB/img/$1.img"            # sparse — costs ~0 bytes
  local dev; dev=$(losetup --find --show "$LAB/img/$1.img")
  mkfs.xfs -q -L "$1" "$dev"
  mkdir -p "$LAB/mnt/$1"
  mount "$dev" "$LAB/mnt/$1"
  echo "$dev"
}

create_disk parity1 8T
for i in 1 2 3 4 5; do create_disk "disk$i" 4T; done
create_disk cache 1T

mergerfs -o category.create=mspmfs,moveonenospc=true,minfreespace=50G \
  "$LAB/mnt/disk1:$LAB/mnt/disk2:$LAB/mnt/disk3:$LAB/mnt/disk4:$LAB/mnt/disk5" \
  "$LAB/mnt/user"
```

**What this gives you, for real, not simulated:**

- Actual mergerfs create-policy behaviour — where files land under `epmfs` vs `mfs`
- Actual `snapraid sync`, `diff`, `scrub`, and `fix`
- Actual parity reconstruction after destroying a "disk"
- Actual `moveonenospc` behaviour when a disk fills
- Actual mover behaviour between cache and array

**What it does not give you:** SMART data, spindown, real IO timing, hardware failure modes. L3 and the proxies in §6 cover what can be covered; the rest is stated residual risk.

**The lab is namespaced and self-guarding** (Q45). Every image, mount point and container name carries `HOSERVA_LAB_ID`, so two labs — two developers' shells, or two agent lanes from the `orchestrate` skill — never collide. Every script refuses to operate on any device that is not a loop device whose backing file lives under its own `$LAB/img/`. `losetup -D` (detach *all*) is never used anywhere.

The single-mount `mergerfs` line above is the minimal starter. The harness grows the per-share topology from doc 02 §1 once spike S6 validates it.

### Failure injection

The harness's real power is in making disasters reproducible:

```bash
# Kill a disk mid-operation
losetup -d /dev/loop3

# Corrupt a disk's contents to test scrub detection — target a known
# file's own extent (via `xfs_bmap -v`), not a blind offset: on a data
# disk this small, a blind `seek=100` can land on XFS's superblock or an
# early allocation-group header instead of inside a file, turning this
# into a filesystem-corruption test rather than the silent-bit-rot test
# scrub is for (spike S5, doc 08 §5). Unmount before writing and remount
# after, so the next read cannot be served from a stale, still-cached
# clean page instead of the now-corrupted on-disk bytes.
umount $LAB/mnt/disk2
dd if=/dev/urandom of=/dev/loopN bs=1 seek=<byte offset inside a target file's own extent> count=500000 conv=notrunc
mount /dev/loopN $LAB/mnt/disk2

# Fill a disk to test moveonenospc
fallocate -l 3.9T $LAB/mnt/disk1/filler

# Simulate the unmounted-disk case the threshold guard must catch
umount $LAB/mnt/disk3

# Make a disk read-only
losetup -d /dev/loop2 && losetup --read-only --find --show $LAB/img/disk2.img
```

Each of these becomes a test case. The threshold guard (doc 02 §2) in particular **must** have a test that fills the array, unmounts a disk, runs a diff, and asserts the sync is blocked. That single test protects the most important safety property in the product.

### Synthetic data generation

Realistic test corpora matter, because `epmfs` behaviour and mover performance depend on file size distribution:

```bash
scripts/devenv/seed-data.sh --profile media      # few, huge files
scripts/devenv/seed-data.sh --profile photos     # many, medium
scripts/devenv/seed-data.sh --profile appdata    # many, tiny, hot
scripts/devenv/seed-data.sh --profile mixed
```

### Running it

Loop devices and mounts need `CAP_SYS_ADMIN`, so the harness runs in a container, never bare on the dev machine. **Not a `--privileged` container, and never with `/dev` bind-mounted**: that exposes every host block device, and one mistyped path formats the developer's real disk. The lab container gets exactly loop devices and FUSE (Q45, validated by spike S9):

```yaml
# docker-compose.dev.yml — started only via `make lab-up`
services:
  lab:
    build: ./scripts/devenv
    container_name: hoserva-lab-${HOSERVA_LAB_ID:?}
    cap_add: [SYS_ADMIN]
    devices:
      - /dev/fuse
      - /dev/loop-control
    device_cgroup_rules:
      - "b 7:* rmw"                # loop block devices (major 7) only — no NVMe, no SATA
    security_opt:
      - apparmor=unconfined        # mount(2) inside the container on AppArmor hosts
    volumes:
      - .:/src
      - ./.lab/${HOSERVA_LAB_ID:?}:/lab/${HOSERVA_LAB_ID:?}
    command: sleep infinity
```

The entrypoint `mknod`s `/dev/loop0..N` inside the container, since the host's udev-created nodes are not visible there. Loop devices are still a host-global resource, which is why the namespacing and the "only loop devices backed by my own image files" guard above are mandatory, not tidiness.

Starting the lab needs access to the Docker daemon, which is root-equivalent on the host; that is the reason labs are only ever started through `make lab-up`, whose recipe is reviewed, never with an ad-hoc `docker run`.

On macOS or Windows, the same container runs inside the Docker VM and works identically. **Development is not tied to a Linux desktop.**

### Teardown

`scripts/devenv/destroy-array.sh` unmounts everything under its own `$LAB`, detaches exactly the loop devices whose backing files are its own images (resolved via `losetup -j`), deletes the images and `.lab/<id>` **from inside the container**, and only then removes the container. Idempotent, and safe to run when things are half-broken — which is the normal state during development. It never touches another lab's devices.

Deleting from inside matters: everything the lab creates in its bind mount is owned by root, so once the container is gone the developer's own user cannot remove it — and an agent's scratch clone containing a leftover `.lab/` cannot be deleted either (found in spike S9, doc 08).

The recipe above is validated on the primary dev host (S9, doc 08): loop devices, XFS and mergerfs work, and the host NVMe's device node cannot be opened from inside the container.

---

## 4. L3 — VM end-to-end

Where loop devices stop, VMs start: real block devices, real boot, real install, real UI.

### Base VM

- QEMU/KVM under the invoking user's `qemu:///session` — never `qemu:///system`, never root — Debian 13 (Q4), 4 vCPU, 4 GB RAM
- Disk images, snapshots and domain names live under the workspace and carry `HOSERVA_LAB_ID`, so parallel agent lanes never collide and teardown removes only its own domains
- 1 virtual disk for the OS, 7 virtual disks for the array (sparse qcow2, sized realistically)
- Managed via a `Makefile` or Vagrant-equivalent scripts, provisioned with the `.deb` under test

### Snapshots as the reset mechanism

The key to making destructive testing repeatable:

```
make vm-up                 # fresh Debian + Hoserva installed
make vm-snapshot NAME=clean
make vm-snapshot NAME=array-configured
make vm-snapshot NAME=array-with-data
make vm-snapshot NAME=unraid-fixtures    # synthetic Unraid disks for migration testing (§5)
make vm-restore NAME=array-with-data     # seconds, not a reinstall
```

Every destructive test starts from a named snapshot and restores afterwards. A parity-reconstruction test that takes 40 minutes to set up by hand takes 5 seconds to reach.

### What runs here

- Full install from `.deb` on clean Debian, and upgrade from the previous version
- The onboarding and array setup wizards, end to end
- **Disk removal while running** — `virsh detach-disk` yanks a disk from a live VM, which is as close to a real failure as it gets without a screwdriver
- Parity reconstruction after replacing the yanked disk
- Reboot persistence: mounts, pool, containers all come back
- Power-loss simulation: `virsh destroy` mid-sync, then assert recovery on boot
- **Config backup and full restore onto a fresh VM** — this is the "OS is disposable" claim from doc 01 §6, and it must be a routine test, not an assumption
- UI end-to-end with Playwright against the real UI
- **The soak test** (§6) — the long-running run whose diff history tunes the guard thresholds (Q16)

### Playwright suite

Runs against the VM's UI over the network. Covers the critical journeys:

1. First-run onboarding → admin created → system check passes
2. Array setup wizard → array created → dashboard shows healthy
3. Add a disk → capacity grows → no rebuild triggered
4. Trigger a sync → diff shown → sync completes → parity fresh
5. **Delete many files → diff shows removals → sync is blocked → warning is visible**
6. Create a share → mount it over SMB from the test runner → write a file → it appears in the pool
7. Install a container from the catalog → it starts → its data lands in the right place
8. Fail a disk → banner appears → replace flow → reconstruction completes → data verified
9. Export config → wipe VM → fresh install → import config → system matches

Test 5 is the one that must never be allowed to regress.

### Testing Hoserva's own VM management (doc 14)

The L3 test VM already runs on libvirt/QEMU to test Hoserva itself. Testing Hoserva's *own* VM-management feature end to end means running KVM **inside** that VM, for a domain Hoserva-under-test creates — nested virtualization. Whether hosted CI runners support nested KVM (as opposed to the outer `/dev/kvm` access S9 already confirmed) is a Phase 3.5 spike (S10, doc 07 §1); if not, agents run that suite on the development host before every release, the same posture as the rest of L3 (§7 below, Q79). PCI/USB passthrough is exercised in a nested guest with an emulated IOMMU (§6); real IOMMU topology and BIOS behaviour stay stated residual risk.

---

## 5. Testing the Unraid migration

The hardest thing to test, because the source is a system Hoserva doesn't control — so the source is built synthetically.

### Building Unraid fixtures without Unraid

No agent runs Unraid or connects to a real Unraid server — not the maintainer's, not even read-only (D20). Unraid's array disks are plain XFS, btrfs or ext4 filesystems with one top-level directory per share, and its configuration is plain files on the flash drive, so the migration source is **built synthetically**:

1. `scripts/devenv/unraid-fixture.sh` partitions and formats loop disks (L2) or virtual disks (L3) the way Unraid does — partition layout, filesystem and mkfs options per supported version (Q24) — from Unraid's public documentation and its public `webgui` source
2. Seeds share directories with realistic data and varied cache settings, plus `appdata`, `domains` and `system`
3. Writes a matching flash tree — disk assignments, share and user configuration, `plugins/dockerMan/templates-user/*.xml` authored for the fixture (never copied from any catalog), and `libvirt.img` for VM variants — packed as a Flash Backup zip
4. Records per-disk file lists, sizes and sha256 as the fixture's expected result
5. **Snapshots** the result (`unraid-fixtures`) so every migration test restores it in seconds

**Optional calibration.** If the maintainer places an Unraid **Diagnostics** zip (Tools → Diagnostics) in `~/.local/share/hoserva/calibration/`, agents compare the fixtures' partition layout, filesystem parameters and config file shapes against it and record any divergence in doc 05 as general layout facts — never values copied from the bundle. The directory sits outside every repository and workspace, because a bundle that isn't anonymised holds hostnames, addresses, disk serials, user and share names and logs. The orchestrator names it in a dispatch as a read-only path; nothing from it is copied into a workspace, committed, or quoted in an issue, comment or commit message, and no agent ever fetches anything from the server. Without it, the fixtures rest on public sources alone, and quirks of disks Unraid itself formatted are stated residual risk.

### Variant fixtures

Per doc 05 §2, build a fixture for each variant that must be supported or explicitly declared unsupported:

- `unraid-6.12-xfs-single-parity` — the primary path
- `unraid-7x-xfs-single-parity` — verify the flash config layout hasn't moved (Q24)
- `unraid-dual-parity` (Q19)
- `unraid-btrfs-and-ext4-disks` — adoption with per-filesystem read-only checks (Q23)
- `unraid-corrupt-xfs` — one disk fails `xfs_repair -n`; assert it is refused and the rest proceed
- `unraid-named-pools` — two pools; assert mapping and path flags (doc 04 §5)
- `unraid-no-cache`
- `unraid-encrypted`, `unraid-zfs-disk` — **refusal fixtures**: assert the scan detects and refuses with the right message (Q22, Q23)
- `unraid-with-vms` — a handful of domains in `libvirt.img` on the array, at least one with a passthrough device referenced; feeds doc 07 Phase 3.5's definition of done and spike S11 (doc 14 §5)

### Migration test procedure

1. Restore the fixture snapshot; its recorded file counts, sizes, checksums and Flash Backup zip are the expected result
2. Attach the fixture's data disks to a fresh VM
3. Install Debian + Hoserva
4. Run `hoserva migrate scan --flash-backup <zip>`, assert the report matches the known array; repeat once with the fixture's flash image attached read-only and assert it was not written (Q25)
5. Run the import, assert **every file is present with matching checksums**
6. Assert share structure and permissions survived
7. Convert templates, assert containers start and find their data
8. Initialise parity, run a scrub, assert no errors
9. Delete a file, `snapraid fix` it back, assert the checksum matches

Step 5 is the one that decides whether this feature ships. Checksums, not file counts — a file that is present but truncated is worse than one that is missing, because nobody notices.

### A hard rule

**Never test the migrator against a real user array, including the maintainer's.** Agents never do (D20); a user does so only through the released product, with a verified backup and a written rollback. The migrator's failure mode is losing 24 TB. The temptation to "just try it on the real box" is exactly how that happens.

---

## 6. Behaviour only hardware shows — proxies and residual risk

There is no hardware test layer (D20): no test box, no testing on the maintainer's homelab or Unraid server, no maintainer-run step. Each behaviour that physical disks would show gets the closest agent-runnable proxy, and what the proxy cannot prove is written down rather than assumed.

| Behaviour | Agent-run proxy | Residual risk — not proven |
|---|---|---|
| **Spindown** (Q31) | In the lab and L3, with the daemon, SMART polling and the change journal running: per-disk read and write counters (`/sys/block/<dev>/stat`) stay flat for 30+ minutes of the Q31 scenario, and any IO that does arrive is attributed to a process (fanotify, blktrace). A disk only leaves standby when IO reaches it, so zero IO is the property Hoserva owns | A drive's firmware or controller waking it with no host IO |
| **SMART polling without waking disks** | Real `smartctl -j` output from many drive models as parser fixtures (L1); in L3, assert the poller issues only standby-aware queries and causes no read IO on an idle disk | Firmware that spins up on a SMART query despite `-n standby` |
| **Disk identity** (Q21) | L3 virtual disks with configured WWN and serial, and USB-attached virtual disks with the serial hidden | Enclosures and HBAs that report identity inconsistently |
| **SnapRAID UUID-dependent behaviour** — true "moved" file classification (snapraid.txt §5.5) and the `-U`/`--force-uuid` disk-identity guard | L3, with a full init system and a running `udevd` so `/dev/disk/by-uuid` is populated | In the L2 lab, SnapRAID can never read a data disk's UUID at all — no `udevd`, and Debian's `snapraid` package isn't linked against `libblkid` either (spike S5, doc 08 §5) — so intra-disk moves are always reported as remove+copy instead of moved, and `-U`'s actual trigger (a disk's UUID no longer matching what was last recorded for its mount point) can never fire. Everything else L2 exercises — sync, diff, scrub, fix, undeleting, touch, single- and dual-parity whole-disk reconstruction — is confirmed to match `snapraid.txt` exactly (doc 08 §5) |
| **Reconstruction timing, throughput** | Measured in L3 on realistically sized sparse disks, as relative comparisons between mergerfs options and releases — never absolute numbers | Absolute speeds and thermals on real disks |
| **PCI/USB passthrough** (doc 14 §3) | A nested L3 guest with an emulated IOMMU and emulated PCI and USB devices: group detection, the generated boot-time VFIO configuration, reboot, the device visible in the guest, assignment removal | Real IOMMU/ACS topology, BIOS quirks, GPU reset and reacquisition |
| **arm64** (Q5) | Cross-built in CI; install and the storage suite in L3 under emulation | Real arm64 boards' storage controllers |

### Soak test

The long-running check that is Phase 1's definition of done (doc 07 §1): an L3 VM runs Hoserva through at least 30 nightly chains back to back — sync, scrub, mover — over seeded daily churn that includes mass deletes and renames, with injected failures (a yanked disk, a full disk, power loss mid-sync). Every blocked sync is reviewed and explained, and the run's diff history is what tunes the guard thresholds (Q16).

### Opt-in public beta

The residual risks above are exercised by volunteers on their own hardware, never by the maintainer: an opt-in beta channel (doc 12 §6), with `hoserva diagnostics` bundles collected against a tracking template rather than handled one report at a time. Beta results update the right-hand column — a risk the beta confirms or refutes is recorded, never silently dropped.

---

## 7. CI

**The repository is public, and that decides where code runs** (Q42). A pull request from a fork can run arbitrary code on whatever runner its workflow targets. A self-hosted runner with privileged loop devices and nested virtualisation is exactly the host that must never execute an untrusted PR.

- **Anything that executes pull-request code runs on GitHub-hosted runners** — they are ephemeral, and their passwordless `sudo` gives loop devices and FUSE for L2.
- **There are no self-hosted runners** (Q79, D20). What hosted runners can't run, agents run on the development host — in the lab and user-session VMs — as a required step before every release.
- **No workflow uses `pull_request_target` to check out PR code.**
- **First-time contributors' workflows require approval** (repository setting).
- Spike S9 confirms hosted runners support loop devices and FUSE — `ci.yml`'s own `lab` job has run L2 successfully in production (run 34950031773). `/dev/kvm` support is not yet confirmed: a probe workflow exists (`.github/workflows/s9-hosted-probe.yml`, doc 08 §9) but has never executed, since triggering it needs a maintainer push. Until that result exists, L3 stays on the dev host (Q79).

### Pipeline

| Stage | Where | When |
|---|---|---|
| Lint, vet, unit tests (L1) | Hosted | Every push and PR |
| Golden-file config diff | Hosted | Every push and PR |
| API contract checks — spec lint, generated code up to date, breaking-change diff (D18, Q63) | Hosted | Every push and PR |
| Frontend build + component tests | Hosted | Every push and PR |
| Loop-device integration (L2) | Hosted (`sudo`, ephemeral) | Every push and PR |
| Schema-migration fixture upgrade (D16) | Hosted | Every push and PR |
| `.deb` build (amd64 + arm64) | Hosted | Every push and PR |
| VM end-to-end (L3) | Hosted if S9 allows; otherwise agents on the dev host | Nightly on `main` where hosted; before every release |
| Hoserva's own VM-management suite (Phase 3.5, nested KVM) | Hosted if S10 allows; otherwise agents on the dev host | Nightly on `main` where hosted; before every release |
| Migration suite | Hosted if S9 allows; otherwise agents on the dev host | Nightly on `main` where hosted; before every release |
| Playwright | Hosted if S9 allows; otherwise agents on the dev host | Nightly on `main` where hosted; before every release |

### Merge gate

L1 + L2 + `.deb` build must pass on every push to `main` and on every external PR. With agent-driven development, work lands as locally verified commits pushed by the maintainer (doc 12 §6), so a red push is fixed forward immediately. L3 is nightly where hosted runners can run it, because a 30-minute VM suite on every push kills iteration speed — but a red nightly, or a missing pre-release agent run, blocks the next release.

### Release checklist, automated where possible

- All test layers green
- Migration suite green on every supported variant
- Config backup/restore round-trip verified
- Upgrade from the previous version verified
- No breaking API change since the previous release without a new API version (oasdiff)
- `.deb` installs cleanly on a fresh Debian
- Template converter clean-conversion rate has not regressed
- Spindown acceptance test (§6 zero-IO proxy) and soak test green
- Every suite hosted runners can't run was run by agents on the development host against the release commit (Q79)

---

## 8. Frontend development

The frontend must be developable without any of the above running.

**Mock API server** — a Go binary implementing the same generated server interfaces as `hoservad` (D18), backed by fixtures, with scenarios selectable by flag:

```
go run ./cmd/mockapi --scenario=healthy
go run ./cmd/mockapi --scenario=degraded
go run ./cmd/mockapi --scenario=rebuilding
go run ./cmd/mockapi --scenario=sync-blocked
go run ./cmd/mockapi --scenario=fresh-install
go run ./cmd/mockapi --scenario=migration-pending
```

This lets UI work happen on any machine with `npm run dev`, and it makes the hard-to-reach states — degraded array, blocked sync, mid-rebuild — trivially reachable for design work. Those are exactly the screens that matter most and that would otherwise be designed blind.

**Fixtures are shared with backend tests**, so the mock cannot drift from reality. If the API response shape changes, both break together — and because the mock implements the generated interfaces, a spec change it doesn't follow fails to compile.

**Component development** in Storybook or equivalent for the complex pieces: disk tiles, capacity visualisations, diff viewer, job progress.

---

## 9. Repository layout

The authoritative layout is doc 12 §2. The testing-specific parts:

```
scripts/devenv/         loop-device harness: create-array.sh, destroy-array.sh, seed-data.sh, inject-failure.sh
scripts/vm/             the L3 *test* VM harness (lifecycle, snapshots, provisioning) — not Hoserva's own
                        VM-management feature, which lives in internal/vm/ (doc 14)
testdata/configs/       golden files
testdata/parsers/       real-world tool output corpus
testdata/unraid-templates/  project-authored Unraid XML template corpus (§2)
web/fixtures/           API fixtures shared by the mock server and backend tests
.lab/                   gitignored per-lab image and mount roots
```

---

## 10. Development loop, in practice

**Backend work:**
```bash
export HOSERVA_LAB_ID=dev        # agents use their unit id, e.g. 57-a1
make lab-up                      # loop-device array in the narrowed lab container
make dev                         # run hoservad against the lab
# edit, rerun, assert against a real mergerfs pool
make lab-destroy
```

**Frontend work:**
```bash
go run ./cmd/mockapi --scenario=degraded &
cd web && npm run dev
```

**Anything destructive or hardware-adjacent:**
```bash
make vm-restore NAME=array-with-data
make vm-deploy                   # build .deb, install into the VM
# test, break things, restore
```

**Before opening a PR:**
```bash
make test                        # L1 + L2
make lint
```

The point of all of this: **the inner loop stays on the dev machine and stays fast.** Loop devices give real storage-engine behaviour in seconds. VMs exist for the cases where reality genuinely differs. What neither can reproduce is stated as residual risk (§6), never tested on someone's own hardware.
