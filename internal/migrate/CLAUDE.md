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
  reported by name and count. A template is read for its `<Name>` only (its
  settings carry secrets), a User Script's `script` and an agent's file are never
  read, `config/shadow` and `config/smbpasswd` are never read, and the lines of
  `smb-extra.conf` and `go` are counted, not quoted. What the later steps seed
  from is `Report.Import`, kept in the session row with the report and not served
  by the API. A file or key that is absent reads as "not available" or "not
  found", never as none or off, and a config is called an orphan only when every
  disk it could be on was matched and listed: keeping an orphan is cheaper than
  dropping a real share.
