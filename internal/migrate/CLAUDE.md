# internal/migrate

The Unraid migrator (doc 05). Every rule here exists because a wrong but plausible
implementation of this package loses a user's data.

- **Never destructive before the point of no return** (doc 05 §5). The scan, the
  review and the import's adoption steps only read from Unraid's disks and flash.
  Nothing in this package formats, repartitions, writes to, or deletes anything on
  a disk, or modifies the Flash Backup zip or a stick it came from. The first step
  that changes a disk is the parity initialisation, behind the user's explicit
  confirmation, and it is not in this package's scan.
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
  shares (`mount -U` would be ambiguous). It is mounted through
  `disk.ReadOnlyMounter` only (never `external.go`'s read-write path), by UUID, as an
  argv, at `<Dir>/stick`, and a mount counts only once the kernel's mount table
  shows it `ro` on both the mount and the superblock. It stays mounted for one read
  (the inspect when queuing, and the job's scan) and is unmounted after each; a
  stick that cannot be unmounted fails the scan, and the next mount refuses until
  the leftover is released, which `Recover` also tries at start. Nothing is copied
  from it, so a stick source has no file: the scan record's file is
  `device:<path>`, and `Forget` has nothing of it to remove. The lab test
  (`stick_lab_test.go`) hashes the whole device before, during and after a scan,
  because FAT's dirty bit goes back on a clean unmount and a read-write mount
  would otherwise leave no trace afterwards.
- **An internal-boot server's zip is the only source** (Q25). The report keeps the
  capture's boot mode; `internal` offers no stick and refuses a stick scan, and so
  does a stick whose own capture says `internal`, which is a copy Unraid no longer
  writes.
