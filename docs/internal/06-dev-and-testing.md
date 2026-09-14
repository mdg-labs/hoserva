# Hoserva — Development Workflow and Testing

## The problem

Hoserva partitions disks, mounts filesystems, writes to `/etc`, and runs as root. **It cannot be installed on a development machine.** Running it there would repartition the dev box's drives.

Worse, the interesting behaviour — a disk failing mid-sync, parity reconstruction, a mover run interrupted by power loss — cannot be triggered on real hardware without destroying real data or waiting hours.

The entire development approach has to be built around this constraint from day one. Retrofitting testability onto a tool that shells out to `mkfs` is not feasible.

---

## 1. The four-layer test pyramid

| Layer | Runs on | Speed | What it covers |
|---|---|---|---|
| **L1 — Unit** | Dev machine, any OS | seconds | Config generation, parsers, business logic, API handlers |
| **L2 — Loop-device integration** | Container or VM, Linux | seconds to minutes | Real mergerfs, real SnapRAID, real filesystems, on fake disks |
| **L3 — VM end-to-end** | Local libvirt/QEMU | minutes | Full install, real block devices, disk failure injection, UI flows |
| **L4 — Hardware** | Dedicated test box | hours | Real disks, spindown, SMART, thermals, performance |

Nearly all development happens at L1 and L2. L3 runs in CI and before releases. L4 runs before a release and for anything involving physical disk behaviour.

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

### Parser tests against real-world corpus

`snapraid diff`, `snapraid status`, `smartctl -j`, `docker` output, and Unraid XML all get parsed. Collect real outputs into `testdata/` and test against them — including malformed and edge cases. Unraid XML especially: pull a few hundred templates from the CA feed and assert the converter handles all of them without panicking, with a tracked count of how many convert cleanly vs. with warnings (clean as defined in Q36). **That number is a release metric.**

**The CA corpus is fetched, not committed.** This repository is public, and committing hundreds of third-party templates is exactly the redistribution doc 04 §4 says not to do. `make test-corpus` downloads the feed into a gitignored cache, pinned to a recorded feed commit so the metric is reproducible. Only self-written templates and templates from repositories whose license clearly permits it are committed under `testdata/unraid-templates/`.

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

mergerfs -o category.create=epmfs,moveonenospc=true,minfreespace=50G \
  "$LAB/mnt/disk1:$LAB/mnt/disk2:$LAB/mnt/disk3:$LAB/mnt/disk4:$LAB/mnt/disk5" \
  "$LAB/mnt/user"
```

**What this gives you, for real, not simulated:**

- Actual mergerfs create-policy behaviour — where files land under `epmfs` vs `mfs`
- Actual `snapraid sync`, `diff`, `scrub`, and `fix`
- Actual parity reconstruction after destroying a "disk"
- Actual `moveonenospc` behaviour when a disk fills
- Actual mover behaviour between cache and array

**What it does not give you:** SMART data, spindown, real IO timing, hardware failure modes. Those are L3 and L4.

**The lab is namespaced and self-guarding** (Q45). Every image, mount point and container name carries `HOSERVA_LAB_ID`, so two labs — two developers' shells, or two agent lanes from the `orchestrate` skill — never collide. Every script refuses to operate on any device that is not a loop device whose backing file lives under its own `$LAB/img/`. `losetup -D` (detach *all*) is never used anywhere.

The single-mount `mergerfs` line above is the minimal starter. The harness grows the per-share topology from doc 02 §1 once spike S6 validates it.

### Failure injection

The harness's real power is in making disasters reproducible:

```bash
# Kill a disk mid-operation
losetup -d /dev/loop3

# Corrupt a disk's contents to test scrub detection
dd if=/dev/urandom of=$LAB/img/disk2.img bs=1M seek=100 count=10 conv=notrunc

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

- libvirt/QEMU, Debian 13 (Q4), 4 vCPU, 4 GB RAM
- 1 virtual disk for the OS, 7 virtual disks for the array (sparse qcow2, sized realistically)
- Managed via a `Makefile` or Vagrant-equivalent scripts, provisioned with the `.deb` under test

### Snapshots as the reset mechanism

The key to making destructive testing repeatable:

```
make vm-up                 # fresh Debian + Hoserva installed
make vm-snapshot NAME=clean
make vm-snapshot NAME=array-configured
make vm-snapshot NAME=array-with-data
make vm-snapshot NAME=unraid-source      # for migration testing
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

---

## 5. Testing the Unraid migration

The hardest thing to test, because it needs a real Unraid array as the source.

### Building an Unraid source VM

Unraid boots from USB and validates a license against the stick's GUID. In a VM this means either passing through a real cheap USB stick (`virsh attach-device` with a USB host device) or using a virtual USB device with a readable serial — the trial license covers 30 days, which is enough to build and snapshot a source image.

Once built:

1. Install Unraid to the stick, create an array across virtual disks
2. Seed it with realistic data and directory structures
3. Install a handful of containers so `templates-user/` is populated
4. Create shares with varied cache settings
5. **Snapshot the whole thing** as `unraid-source`

That snapshot is then the fixture for every migration test. Build it once; restore it in seconds thereafter.

### Variant fixtures

Per doc 05 §2, build a snapshot for each variant that must be supported or explicitly declared unsupported:

- `unraid-6.12-xfs-single-parity` — the primary path
- `unraid-7x-xfs-single-parity` — verify the flash config layout hasn't moved (Q24)
- `unraid-dual-parity` (Q19)
- `unraid-btrfs-and-ext4-disks` — adoption with per-filesystem read-only checks (Q23)
- `unraid-corrupt-xfs` — one disk fails `xfs_repair -n`; assert it is refused and the rest proceed
- `unraid-named-pools` — two pools; assert mapping and path flags (doc 04 §5)
- `unraid-no-cache`
- `unraid-encrypted`, `unraid-zfs-disk` — **refusal fixtures**: assert the scan detects and refuses with the right message (Q22, Q23)

### Migration test procedure

1. Restore the source snapshot, record file counts, sizes, and checksums per disk; export the fixture's Flash Backup zip
2. Shut down, detach the Unraid stick, attach a fresh OS disk
3. Install Debian + Hoserva
4. Run `hoserva migrate scan --flash-backup <zip>`, assert the report matches the known array; repeat once with the stick attached read-only and assert it was not written (Q25)
5. Run the import, assert **every file is present with matching checksums**
6. Assert share structure and permissions survived
7. Convert templates, assert containers start and find their data
8. Initialise parity, run a scrub, assert no errors
9. Delete a file, `snapraid fix` it back, assert the checksum matches

Step 5 is the one that decides whether this feature ships. Checksums, not file counts — a file that is present but truncated is worse than one that is missing, because nobody notices.

### A hard rule

**Never test the migrator against a real user array, including your own, without a verified backup and a written rollback.** The migrator's failure mode is losing 24 TB. The temptation to "just try it on the real box" is exactly how that happens.

---

## 6. L4 — Hardware testing

Some things only a real machine shows: spindown, SMART, thermals, actual throughput, controller quirks.

### The test box

An old desktop or a cheap mini PC with 3–4 second-hand disks is sufficient. Small disks are better — a 500 GB disk rebuilds in minutes where an 8 TB one takes hours, and the behaviour is the same.

### What must be tested here

- **Spindown** — doc 02 §1, doc 08 §1. Acceptance criterion (Q31): with no SMB/NFS clients connected, no containers holding pool paths open, and appdata on cache, array disks stay in standby for 30+ minutes. Verified with `smartctl -n standby` polling and the wake-event log; the change journal (Q13) must be running during the test, so it is proven not to wake anything.
- **SMART polling without waking disks** — assert that the monitoring loop itself doesn't defeat spindown, which is a classic self-inflicted bug
- **Real reconstruction timing** — so the UI's ETA estimates are not fiction
- **Thermals** under a full scrub with every disk active
- **Actual throughput** over SMB, to catch mergerfs option mistakes that a loop device would hide
- **Controller behaviour** — HBA and onboard SATA enumerate differently; disk identification must be robust across both

### Beta hardware diversity

Before 1.0, a small beta group running varied hardware will surface more than any lab. What matters is collecting `hoserva diagnostics` bundles systematically rather than handling reports one by one in a Discord channel.

---

## 7. CI

**The repository is public, and that decides where code runs** (Q42). A pull request from a fork can run arbitrary code on whatever runner its workflow targets. A self-hosted runner with privileged loop devices and nested virtualisation is exactly the host that must never execute an untrusted PR.

- **Anything that executes pull-request code runs on GitHub-hosted runners** — they are ephemeral, and their passwordless `sudo` gives loop devices and FUSE for L2.
- **Self-hosted runners run only on trusted triggers** — `push` to `main`, `schedule`, `workflow_dispatch` — never on `pull_request` from forks, and never via `pull_request_target` checking out PR code.
- **First-time contributors' workflows require approval** (repository setting).
- Spike S9 confirms hosted runners support loop devices, FUSE and `/dev/kvm` for the pinned toolchain; if hosted KVM suffices, L3 moves to hosted runners too.

### Pipeline

| Stage | Where | When |
|---|---|---|
| Lint, vet, unit tests (L1) | Hosted | Every push and PR |
| Golden-file config diff | Hosted | Every push and PR |
| Frontend build + component tests | Hosted | Every push and PR |
| Loop-device integration (L2) | Hosted (`sudo`, ephemeral) | Every push and PR |
| `.deb` build (amd64 + arm64) | Hosted | Every push and PR |
| VM end-to-end (L3) | Self-hosted, nested virt — or hosted if S9 allows | Nightly on `main` + pre-release |
| Migration suite | Self-hosted | Nightly on `main` + pre-release |
| Playwright | Self-hosted | Nightly on `main` + pre-release |

### Merge gate

L1 + L2 + `.deb` build must pass on every push to `main` and on every external PR. With agent-driven development, work lands as locally verified commits pushed by the maintainer (doc 12 §6), so a red push is fixed forward immediately. L3 is nightly, because a 30-minute VM suite on every push kills iteration speed — but a red nightly blocks the next release.

### Release checklist, automated where possible

- All test layers green
- Migration suite green on every supported variant
- Config backup/restore round-trip verified
- Upgrade from the previous version verified
- `.deb` installs cleanly on a fresh Debian
- Template converter clean-conversion rate has not regressed
- Spindown acceptance test passed on the hardware box

---

## 8. Frontend development

The frontend must be developable without any of the above running.

**Mock API server** — a Go binary serving the same API from fixtures, with scenarios selectable by flag:

```
go run ./cmd/mockapi --scenario=healthy
go run ./cmd/mockapi --scenario=degraded
go run ./cmd/mockapi --scenario=rebuilding
go run ./cmd/mockapi --scenario=sync-blocked
go run ./cmd/mockapi --scenario=fresh-install
go run ./cmd/mockapi --scenario=migration-pending
```

This lets UI work happen on any machine with `npm run dev`, and it makes the hard-to-reach states — degraded array, blocked sync, mid-rebuild — trivially reachable for design work. Those are exactly the screens that matter most and that would otherwise be designed blind.

**Fixtures are shared with backend tests**, so the mock cannot drift from reality. If the API response shape changes, both break together.

**Component development** in Storybook or equivalent for the complex pieces: disk tiles, capacity visualisations, diff viewer, job progress.

---

## 9. Repository layout

The authoritative layout is doc 12 §2. The testing-specific parts:

```
scripts/devenv/         loop-device harness: create-array.sh, destroy-array.sh, seed-data.sh, inject-failure.sh
scripts/vm/             VM lifecycle, snapshots, provisioning
testdata/configs/       golden files
testdata/parsers/       real-world tool output corpus
testdata/unraid-templates/  committable XML corpus (the CA corpus is fetched, §2)
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

The point of all of this: **the inner loop stays on the dev machine and stays fast.** Loop devices give real storage-engine behaviour in seconds. VMs exist for the cases where reality genuinely differs. Hardware exists for the handful of things neither can fake.
