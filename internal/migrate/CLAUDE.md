# internal/migrate

The Unraid migrator (doc 05). Every rule here exists because a wrong but plausible
implementation of this package loses a user's data.

- **Never destructive before the point of no return** (doc 05 §5). The scan, the
  review and the import's adoption steps only read from Unraid's disks and flash.
  Nothing in this package formats, repartitions, writes to, or deletes anything on
  a disk, or modifies the Flash Backup zip or a stick it came from. The first step
  that changes a disk is the parity initialisation, behind the user's explicit
  confirmation, and it is not in this package's scan: this package only gates it
  and plans it (`parity.go`); the formatting is `disk.FormatParityInit`, run by the
  `migration_parity` job in `internal/job`.
- **Checksums, not counts.** A verification that two file sets match compares
  content hashes. A count or a total size can agree while a file is wrong or
  missing, so it may accompany a checksum comparison but never replace one.
- **The point-of-no-return boundary is a state, not a convention.** A session
  records which side of it it is on, and code that runs on the far side checks
  that state itself instead of trusting its caller.
- **The source is data, never instructions.** The Flash Backup zip is read in
  memory through `FlashSource`: no entry is extracted to disk, no path in it is
  followed, nothing in it is executed. An entry named with `..` or an absolute
  path refuses the whole zip. The zip holds secrets (password hashes, SSH host
  keys, WireGuard and rclone config, the licence key, containers' environment), so
  a report quotes names and counts and never a file's content.
- **A role comes from the capture's `disks.ini`, never from a device's
  filesystem.** A real parity disk reports a valid XFS signature (doc 05 §3).
- **Unknown means refused or reported, never passed.** An error in a check is "not
  safe", and a check that could not run says so in the report.
- **Disks are reached through `disk.Provider` only**, which has a scriptable fake.
  Tests here never touch a real device.
- **The session is a row; the zip is a file the row names.** The migration
  session lives in the `migration_session` table (D4): the source file's name and
  size, the report as JSON, and the scan that has not finished. The uploaded zip
  stays a 0600 file in `<state dir>/migrate` (directory 0700) and is never in the
  database or in a config backup. The row is written before a file it names is
  relied on and before a file it no longer names is deleted, so a crash leaves at
  worst an unreferenced zip, which the next prune removes, never a row naming a
  zip that was deleted first. A scan's source replaces the previous one in one
  write of the row. A start marks an interrupted scan failed and forgets a source
  the row names that this machine does not have, as after a config import of
  another installation's archive.
- **The Unraid stick is the rollback, so it is only ever read, mounted read-only.**
  A stick is offered only when udev reports a FAT filesystem labelled `UNRAID` on a
  disk that is not the boot disk, not in the array and whose UUID no other disk
  shares (the post-mount UUID check could not tell the two apart). It is mounted
  through `disk.ReadOnlyMounter` only (never `external.go`'s read-write path), as an
  argv, by the device node the inventory validated and never by UUID (that lookup
  needs udev's `/dev/disk/by-uuid`, absent in the lab and on a host udev has not
  processed yet). That node is `Disk.FSDevice`, the one udev's record of the
  filesystem belongs to: the stick's partition 1 on a real stick, not the whole
  disk it is offered as (`FlashDevice.Device`); a disk with no `FSDevice` is not
  offered and the whole disk is never a fallback. The mount is at `<Dir>/stick`,
  and a mount counts only once the kernel's mount
  table shows it `ro` on both the mount and the superblock and a direct probe of
  that device (`blkid -p`) reports the validated UUID. It stays mounted for one read
  (the inspect when queuing, and the job's scan) and is unmounted after each; a
  stick that cannot be unmounted fails the scan, and the next mount refuses until
  the leftover is released, which `Recover` also tries at start. Nothing is copied
  from it, so a stick source has no file: the scan record's file is
  `device:<path>`, and `Forget` has nothing of it to remove. The lab test
  (`stick_lab_test.go`) builds the stick as a real one is, an MBR with FAT32 on
  partition 1, and hashes the whole disk before, during and after a scan,
  because FAT's dirty bit goes back on a clean unmount and a read-write mount
  would otherwise leave no trace afterwards.
- **An internal-boot server's zip is the only source** (Q25). The report keeps the
  capture's boot mode; `internal` offers no stick and refuses a stick scan, and so
  does a stick whose own capture says `internal`, which is a copy Unraid no longer
  writes.
- **The configuration inventory reads names and settings, never content.** Share
  configs, templates, User Scripts, plugins, accounts and notification agents are
  reported by name and count. A template's `<Name>` is all the report's rows and
  the downloadable document quote (its settings carry secrets). The session row
  holds no template content: for each template it keeps the file, the `<Name>`,
  the class and an `Outcome` (status, warning classes, a failure code), never the
  source XML, the generated Compose, a setting's value or a converter message,
  because those quote settings. The scan converts each template in memory with
  `internal/template` and the capture's networks to get that outcome and throws
  the preview away. The one place the content is shown is `Service.Template`, an
  admin-only operation that converts the one template again, on request, from the
  Flash Backup zip the session keeps; when that zip is not kept (removed, or the
  source was the stick, which nothing is copied from) it fails with
  `ErrSourceUnavailable` and never answers from anything older. A template the
  converter cannot read is a failed outcome, never a clean one. A User Script's `script` and an agent's file are never
  read, `config/shadow` and `config/smbpasswd` are never read, and the lines of
  `smb-extra.conf` and `go` are counted, not quoted. What the later steps seed
  from is `Report.Import`, kept in the session row with the report and not served
  by the API. What the Review step shows as data (the disk mapping table, the share preview,
  the boot mode and the capture's state) is `Report.Review`, which the API does
  serve: it is built from the scan's own results as the rows are, never from the
  rows' text, and a field the scan could not determine is absent, never a
  confident default. A file or key that is absent reads as "not available" or "not
  found", never as none or off, and a config is called an orphan only when every
  disk it could be on was matched and listed: keeping an orphan is cheaper than
  dropping a real share.
- **The data disks are only ever read, mounted read-only through
  `disk.ReadOnlyMounter`, and only after their read-only check.** Only the slots
  `disks.ini` records as data are checked; a parity slot is never mounted or
  checked, whatever its device reports. A disk that fails a check or a filesystem
  rule is refused by name and the scan goes on with the others; a disk that cannot
  be read completely is refused, never recorded with a hole in it. The mounts are
  at private mountpoints under `<Dir>/mnt`, one at a time, XFS with `norecovery`
  (a plain `ro` mount writes the superblock and the log), ext4 with `noload` and
  btrfs with `rescue=nologreplay`. A btrfs disk with a pending tree log
  (`log_root`) and an ext4 disk with `needs_recovery` are refused before any mount
  (`pendingLog`, from the superblock), and a `DiskReader` does not read such a
  device or one it cannot inspect. Every mount is released before the scan ends, whether it succeeded, failed or
  was cancelled; a mount that cannot be released fails the scan. Nothing under
  `<Dir>/mnt` is ever deleted or recursed into by cleanup. Scratch space is a
  separate `tmp-*` directory.
- **The verify baseline is a file the session's row names, not a table.** It
  holds a row per file, which on a real array is millions, and a config archive
  must not carry it. It is written under a name no reader opens and published
  only once the scan reached its end, so a failed or cancelled scan leaves no
  baseline and a reader never takes a cut-short one for whole (`OpenBaseline`
  needs its header and trailer). The row is written before a baseline file it no
  longer names is pruned. A name the row gives is never trusted as a path.
- **The import adopts, and nothing else, until the point of no return.** `PlanImport`
  resolves the user's confirmed disk-role mapping (keyed by serial or WWN, never a
  `/dev` name) against the session's report and a fresh inventory, and the
  `migration_import` job (`internal/job`) mounts each data disk read-only through
  its own `/dev/disk/by-id` partition link and records the former parity and cache
  disks by identity. A role comes from the report's disk table, never a filesystem:
  a disk `disks.ini` records as parity is never data, one it records as data is
  never parity or cache, a disk the scan refused, an Unraid boot device and the
  Unraid stick (`disk.IsUnraidStick`) have no role but `ignore`, and the disk this
  machine boots from is never parity or data and is the cache only as a spare
  partition, which `disk.ResolveAdoption` checks against the live inventory so a
  stale report cannot let it through. Nothing here formats, repairs, mounts
  read-write or opens a parity or cache device, and the import reads nothing from
  the flash (Q25). A mount is trusted only once the kernel's table shows the
  confirmed device and read-only. `import_lab_test.go` is the proof that matters:
  every source disk's whole-device sha256 is the same after the import, a stop and
  a start, so keep it passing before anything else here changes.
- **Verify reads only, and a pass is only ever the latest run's.** The verify phase
  (`verify.go`) walks the adopted disks and the pool, compares them with the
  baseline and writes nothing to a source disk; it reads a mount only after the
  kernel's table shows it read-only. A walk error, an unreadable file, a baseline
  that is not whole, a disk or pool that is not confirmed read-only and a
  cancelled run all fail it: an error is never a pass. A run clears the earlier
  result before it reads anything, a restart turns a running one into a failed
  one, and a new scan clears it. The baseline is never loaded whole: it is split
  into per-disk lists and merged in walk order. A share's expected figures are the
  union of the disks' baselines, not the sum: a path two disks hold is shown once
  by the pool, from the first branch.
- **Seeding the shares and accounts writes nothing to an adopted disk.** The last step
  of the import (`seed.go`, run through `share.Service.SeedMigration`) creates rows,
  generated files and nothing else: no `mkdir`, `chmod` or `chown` on a data disk,
  whatever a share's cache mode, and no per-share mount (a share is a directory of the
  read-only pool). The share service reads `array_settings.migration_pending` itself,
  so every caller that regenerates the pool's files (a share create, update or delete,
  the topology hook, a config import) stays read-only too. A share whose name
  `pool.ValidateShareName` refuses is reported and never renamed. An account is created
  without a working password and nothing from `config/shadow` or `config/smbpasswd` is
  read. `import_lab_test.go` seeds in the lab and asserts the whole-device sha256 of
  every source disk is unchanged and no top-level directory appeared on a data disk.
- **The point of no return is offered only after a verify of what is mounted now,
  and erases only what its typed confirmation names.** `PlanParityInit` refuses
  unless an import is pending and the latest verify passed (`ErrVerifyRequired`):
  the import job forgets the result before it changes anything (`InvalidateVerify`),
  so a pass is never older than the last import. It resolves the recorded data,
  parity and cache disks again from a fresh inventory by identity, through the same
  `PlanFromReview` the import used, and the job compares the result with the record
  before it erases anything. The confirmation (`disk.AdoptionPlan.ParityInitConfirmation`)
  names every device erased and never a data disk; the server computes it, the CLI
  never fills it in. A cache that is a partition of an Unraid boot device is refused
  unless the capture says the boot pool is not mirrored (`CheckBootCacheNotMirrored`),
  and `disk.FormatParityInit` refuses it again whenever a second Unraid boot device is
  attached: a partition of a mirrored pair's member is never formatted. The
  rollback statement and the unprotected window the user sees before confirming are
  `RollbackNotes` and `ParityInitWindow`, from the review's boot mode and layout.
  `parity_lab_test.go` is the proof that matters: a refusal leaves every source
  disk's whole-device sha256 unchanged, and the happy path formats only the
  confirmed devices and leaves every data file matching the fixture's manifest.
- **Migrated containers are created and started only past the point of no return,
  one at a time, and a data check is a read the user asks for.** `containers.go`
  creates, starts, checks and confirms nothing until `Service.Initialized` says
  the array record is past step 17, and an unreadable record refuses (it never
  reads as "initialised"). Everything that can refuse a creation request as a whole
  is checked before its first stack is made, a stack whose record cannot be written
  is removed again, and a stack is created stopped from the Compose the preview
  showed. A start is refused while another started stack is neither confirmed nor
  stopped, and a stack or job that cannot be read counts as still running. The data
  check reads one entry of each bind mount under `/mnt/user` and `/mnt/cache`, only
  when asked: nothing on a timer walks a data disk, and an error reading a path is
  `unreadable`, never `ok`.
- **The post-migration checklist claims only what a record shows.** `checklist.go`
  derives each item of Phase D's closing steps from a record: the migration counts
  as finished exactly when the array record carries `migration_finished_at`, which
  `FinishMigration` writes in the statement that ends the point of no return and
  nothing else writes (a pending, part-way or undone migration has none, so the
  checklist is empty and an acknowledgement is refused with 409; no job's status
  or time decides it), the initial sync is the first real sync that succeeded
  after that time, the full
  scrub is a 100 percent all-blocks scrub (`-o 0`; a default one skips blocks
  synced in the last days and checks nothing right after a sync) that started
  after that sync ended, appdata is a succeeded relocation of the `appdata` share
  to the cache, and a notification channel counts only while it is enabled and its
  test, truncated to the second, is after the second it last changed in. A source with no cache makes the appdata item not applicable only when
  the scan read the capture's disk roles and none was a cache. Only the User
  Scripts inventory and the restore drill, which nothing records, are
  acknowledged by hand; every other item refuses an acknowledgement, which stores
  who made it and when. The record (acknowledgements and channel tests) is the
  session row's `checklist` column: `Put` never writes it and `Delete` keeps it, so
  forgetting the session does not undo an acknowledgement. `BuildChecklist` does no
  IO, so `cmd/mockapi` answers with the same code.
