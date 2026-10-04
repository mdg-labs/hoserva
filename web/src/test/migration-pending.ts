import type { components } from "@/lib/api/client";

// The answers cmd/mockapi gives for GET /migrate and GET /migrate/templates in
// its migration-pending scenario (a no-go report with a refused disk, flagged
// containers, one clean and one warning template, a Compose project and an
// Unraid stick), with the structured review (a refused disk, a weak-identity
// disk, a slot with no disk here, a High-water share and a fresh capture),
// kept as they are served so the page tests run against the shape the
// scenario's fixtures produce.
export type Migration = components["schemas"]["Migration"];
export type MigrationReportRow = components["schemas"]["MigrationReportRow"];
export type MigrationTemplates = components["schemas"]["MigrationTemplates"];

export const migrationPending: Migration = {
  "phase": "scanned",
  "sourceSize": 641728512,
  "sourceReceivedAt": "2026-10-03T15:51:52Z",
  "flashDevices": [
    {
      "device": "/dev/sdu",
      "size": 17179869184,
      "model": "SanDisk Cruzer Fit",
      "serial": "4C530001240603119335"
    }
  ],
  "zipOnly": false,
  "report": {
    "generatedAt": "2026-10-03T15:51:52Z",
    "unraidVersion": "7.3.2",
    "unverifiedLayout": false,
    "verdict": "no_go",
    "rows": [
      {
        "check": "unraid_version",
        "status": "pass",
        "detail": "Unraid 7.3.2, flash layout recognised."
      },
      {
        "check": "boot_device",
        "status": "info",
        "detail": "Unraid boots from a USB stick. Keep the stick: it is the rollback."
      },
      {
        "check": "disk_mapping",
        "status": "pass",
        "subject": "parity",
        "detail": "serial EXAMPLE_PARITY is Unraid parity slot 0; this machine has it as /dev/sdb, 8.0 TiB."
      },
      {
        "check": "disk_mapping",
        "status": "pass",
        "subject": "disk1",
        "detail": "serial EXAMPLE_DISK1 is Unraid disk 1 (xfs); this machine has it as /dev/sdc, 4.0 TiB."
      },
      {
        "check": "disk_mapping",
        "status": "flag",
        "subject": "disk2",
        "detail": "Unraid had a disk with serial EXAMPLE_DISK2 here, and no disk on this machine has this serial or WWN. Attach it before the import."
      },
      {
        "check": "disk_mapping",
        "status": "pass",
        "subject": "disk3",
        "detail": "serial EXAMPLE_DISK3 is Unraid disk 3 (xfs); this machine has it as /dev/sdd, 4.0 TiB."
      },
      {
        "check": "disk_mapping",
        "status": "pass",
        "subject": "disk4",
        "detail": "serial EXAMPLE_DISK4 is Unraid disk 4 (xfs); this machine has it as /dev/sde, 2.0 TiB."
      },
      {
        "check": "disk_mapping",
        "status": "pass",
        "subject": "pool cache",
        "detail": "serial EXAMPLE_CACHE is a pool device; this machine has it as /dev/nvme0n1, 500.0 GiB."
      },
      {
        "check": "disk_identity",
        "status": "pass",
        "subject": "parity",
        "detail": "/dev/sdb has a WWN or serial."
      },
      {
        "check": "disk_identity",
        "status": "flag",
        "subject": "disk4",
        "detail": "/dev/sde has only a weak identity (a USB enclosure hides its serial): it can be a data disk, matched by filesystem UUID and size (Q21)."
      },
      {
        "check": "data_disks",
        "status": "info",
        "subject": "parity",
        "detail": "A parity slot: /dev/sdb is never mounted or checked, and its role comes from its slot, never from its filesystem. Its device reports xfs: a parity disk's bytes are an XOR of the data disks', which can leave a valid-looking superblock."
      },
      {
        "check": "data_disks",
        "status": "pass",
        "subject": "disk1",
        "detail": "xfs on /dev/sdc, a single filesystem; the flash and the device agree on it."
      },
      {
        "check": "data_disks",
        "status": "pass",
        "subject": "disk3",
        "detail": "xfs on /dev/sdd, a single filesystem; the flash and the device agree on it."
      },
      {
        "check": "data_disks",
        "status": "pass",
        "subject": "disk4",
        "detail": "xfs on /dev/sde, a single filesystem; the flash and the device agree on it."
      },
      {
        "check": "disk_integrity",
        "status": "pass",
        "subject": "disk1",
        "detail": "The read-only xfs check of /dev/sdc is clean."
      },
      {
        "check": "disk_integrity",
        "status": "refuse",
        "subject": "disk3",
        "detail": "disk3 is not adopted: its read-only xfs check failed (xfs_repair -n exit status 1: a bad free-space B-tree block in allocation group 0). Computing parity over a damaged filesystem would keep the damage. An XFS disk that was not unmounted cleanly has a log that needs replaying: start Unraid, stop the array cleanly, and scan again. Hoserva never replays a log on a disk it does not own yet."
      },
      {
        "check": "disk_integrity",
        "status": "pass",
        "subject": "disk4",
        "detail": "The read-only xfs check of /dev/sde is clean."
      },
      {
        "check": "baseline",
        "status": "info",
        "detail": "Recorded for the verify phase, with the session. Content hashes: every file of 1.0 MiB or less is hashed, plus a deterministic 1 in 100 (at least 200) of the larger files on each disk, chosen by a stable hash of the path. Every file's size, every symlink with its target and every special file by type is recorded as well."
      },
      {
        "check": "baseline",
        "status": "info",
        "subject": "disk1",
        "detail": "48210 files (3.6 TiB), 12 symlinks and 0 special files; 31044 files (96.2 GiB) hashed."
      },
      {
        "check": "baseline",
        "status": "info",
        "subject": "media",
        "detail": "41007 files (3.4 TiB), 0 symlinks and 0 special files, on disk1."
      },
      {
        "check": "content_space",
        "status": "pass",
        "detail": "3 content-file copies can be placed (Q18): the boot device, 2 data disks, with room for about 9.4 MiB for the content file (48210 files, a planning figure of 200 bytes per file and 24 per 256 KiB block)."
      },
      {
        "check": "parity_config",
        "status": "info",
        "detail": "1 parity disk(s). Parity is rewritten from scratch either way."
      },
      {
        "check": "parity_size",
        "status": "pass",
        "detail": "The smallest parity disk (8.0 TiB) is at least as large as the largest data disk (4.0 TiB)."
      },
      {
        "check": "smart",
        "status": "pass",
        "subject": "parity",
        "detail": "/dev/sdb has no reallocated or pending sectors."
      },
      {
        "check": "smart",
        "status": "flag",
        "subject": "disk1",
        "detail": "Recommend aborting: /dev/sdc has 8 reallocated and 0 pending sectors, and the unprotected window of the migration is when a marginal disk fails."
      },
      {
        "check": "parity_history",
        "status": "pass",
        "detail": "The last parity check, on 2026-09-25 (from the capture's var.ini), completed clean with 0 errors."
      },
      {
        "check": "shares",
        "status": "info",
        "detail": "3 shares configured. Each share's allocation method and cache setting are mapped below (Q11)."
      },
      {
        "check": "shares",
        "status": "flag",
        "subject": "media",
        "detail": "allocation High-water: no exact equivalent, mapped to Balance across disks (mfs) (Q11); cache setting no maps to array-only; exported over SMB (e); directory on disk1."
      },
      {
        "check": "shares",
        "status": "info",
        "subject": "backup",
        "detail": "allocation Fill-up maps to Fill disks in order (ff); cache setting no maps to array-only; not exported over SMB; never on disk3; directory on disk1."
      },
      {
        "check": "shares",
        "status": "info",
        "subject": "documents",
        "detail": "allocation Most-free maps to Balance across disks (mfs); cache setting yes maps to cache-then-move; exported over SMB (e); directory on disk1."
      },
      {
        "check": "cache_contents",
        "status": "warn",
        "subject": "appdata",
        "detail": "Docker keeps container data under /mnt/user/appdata/; cache setting prefer; its directory is on disk1, pool cache. Phase A step 5 must move it to the array before the cache is re-created."
      },
      {
        "check": "cache_contents",
        "status": "info",
        "subject": "Docker storage",
        "detail": "Docker's directory (/mnt/user/system/docker/dockerdir) is on the cache. It is not moved: images are pulled again when containers are recreated. What does not come back is each container's writable layer (doc 04 §5). No container's writable layer holds data."
      },
      {
        "check": "users",
        "status": "info",
        "detail": "2 user accounts: alice, bob. Names only are read; passwords cannot be carried over, so each is set again at the import (doc 05 §4 step 4)."
      },
      {
        "check": "docker_templates",
        "status": "info",
        "detail": "2 templates parsed: 1 autostart, 1 running, 0 stopped, 0 template only. 2 are installed; a template with no container is a record of an app once installed."
      },
      {
        "check": "docker_templates",
        "status": "info",
        "detail": "Of the 2 installed templates, 1 convert cleanly and 1 with warnings (Q36)."
      },
      {
        "check": "docker_templates",
        "status": "info",
        "subject": "gateway",
        "detail": "Converts with 2 warnings to review (flagged_path, missing_network). Open its preview before recreating the container."
      },
      {
        "check": "containers",
        "status": "info",
        "detail": "1 Compose Manager project previewed with its own compose.yaml and not converted."
      },
      {
        "check": "containers",
        "status": "info",
        "detail": "8 containers in the capture: 6 from the Docker page (dockerMan), 1 from Compose Manager, 1 created by hand."
      },
      {
        "check": "containers",
        "status": "flag",
        "subject": "dbtool",
        "detail": "A dockerMan container with no template whose <Name> matches. It cannot be converted: open it on the Docker page, edit it and apply to save its template, then run the prepare script again (doc 05 §4 step 2)."
      },
      {
        "check": "containers",
        "status": "flag",
        "subject": "handmade",
        "detail": "Created by hand (docker run), so it has no template to convert. Recreate it from its run command."
      },
      {
        "check": "user_scripts",
        "status": "info",
        "detail": "1 User Scripts entry found. They are listed, never executed or translated (Q83): recreate what is still wanted as a cron job or systemd timer (doc 05 §4 step 24)."
      },
      {
        "check": "user_scripts",
        "status": "info",
        "subject": "nightly-report",
        "detail": "Scheduled in customSchedule.cron (30 2 * * *), so it runs while enabled."
      },
      {
        "check": "plugins",
        "status": "info",
        "subject": "user.scripts",
        "detail": "Installed. Hoserva counterpart: its scripts are listed, never executed or translated (Q83)."
      },
      {
        "check": "custom_config",
        "status": "warn",
        "subject": "smb-extra.conf",
        "detail": "2 lines of custom Samba configuration. It is not imported: look at the lines on the Unraid server and recreate any that are still wanted."
      },
      {
        "check": "settings",
        "status": "info",
        "subject": "mover schedule",
        "detail": "40 3 * * *. It can be offered as Hoserva's mover schedule."
      },
      {
        "check": "uid_99",
        "status": "pass",
        "detail": "UID 99 is free for the hoserva-apps user."
      },
      {
        "check": "sync_estimate",
        "status": "info",
        "detail": "About 2.8 hours for 4.0 TiB of data disks, if they are full, at an assumed 400 MB/s. It is a planning figure, not a measurement: the first sync is a long job, and it runs only when you start it."
      }
    ],
    "review": {
      "disks": [
        {
          "slot": "parity",
          "unraidId": "EXAMPLE_PARITY",
          "unraidRole": "parity",
          "proposedRole": "parity",
          "hostBoot": false,
          "device": "/dev/sdb",
          "serial": "EXAMPLE_PARITY",
          "wwn": "0x5000c500a1b2c3d4",
          "byId": "ata-EXAMPLE_PARITY",
          "model": "EXAMPLE 8TB",
          "size": 8796093022208,
          "filesystem": "xfs",
          "weakIdentity": false,
          "refused": false
        },
        {
          "slot": "disk1",
          "diskNumber": 1,
          "unraidId": "EXAMPLE_DISK1",
          "unraidRole": "data",
          "proposedRole": "data",
          "hostBoot": false,
          "device": "/dev/sdc",
          "serial": "EXAMPLE_DISK1",
          "byId": "ata-EXAMPLE_DISK1",
          "model": "EXAMPLE 4TB",
          "size": 4398046511104,
          "filesystem": "xfs",
          "weakIdentity": false,
          "refused": false
        },
        {
          "slot": "disk2",
          "diskNumber": 2,
          "unraidId": "EXAMPLE_DISK2",
          "unraidRole": "data",
          "size": 4398046511104,
          "problem": "no disk on this machine has this serial or WWN",
          "refused": false
        },
        {
          "slot": "disk3",
          "diskNumber": 3,
          "unraidId": "EXAMPLE_DISK3",
          "unraidRole": "data",
          "hostBoot": false,
          "device": "/dev/sdd",
          "serial": "EXAMPLE_DISK3",
          "byId": "ata-EXAMPLE_DISK3",
          "model": "EXAMPLE 4TB",
          "size": 4398046511104,
          "filesystem": "xfs",
          "weakIdentity": false,
          "refused": true,
          "refusalCode": "integrity_check",
          "refusal": "disk3 is not adopted: its read-only xfs check failed (xfs_repair -n exit status 1: a bad free-space B-tree block in allocation group 0). Computing parity over a damaged filesystem would keep the damage. An XFS disk that was not unmounted cleanly has a log that needs replaying: start Unraid, stop the array cleanly, and scan again. Hoserva never replays a log on a disk it does not own yet."
        },
        {
          "slot": "disk4",
          "diskNumber": 4,
          "unraidId": "EXAMPLE_DISK4",
          "unraidRole": "data",
          "proposedRole": "data",
          "hostBoot": false,
          "device": "/dev/sde",
          "serial": "EXAMPLE_DISK4",
          "model": "EXAMPLE USB 2TB",
          "size": 2199023255552,
          "filesystem": "xfs",
          "weakIdentity": true,
          "refused": false
        },
        {
          "slot": "pool cache",
          "unraidId": "EXAMPLE_CACHE",
          "unraidRole": "cache",
          "proposedRole": "cache",
          "hostBoot": false,
          "device": "/dev/nvme0n1",
          "serial": "EXAMPLE_CACHE",
          "byId": "nvme-EXAMPLE_CACHE",
          "model": "EXAMPLE NVMe 500GB",
          "size": 536870912000,
          "filesystem": "btrfs",
          "weakIdentity": false,
          "refused": false
        },
        {
          "slot": "boot",
          "unraidRole": "boot",
          "proposedRole": "ignore",
          "hostBoot": false,
          "device": "/dev/sdu",
          "serial": "4C530001240603119335",
          "model": "SanDisk Cruzer Fit",
          "size": 17179869184,
          "filesystem": "vfat",
          "weakIdentity": false,
          "refused": false
        },
        {
          "unraidRole": "unassigned",
          "hostBoot": false,
          "device": "/dev/sdf",
          "serial": "EXAMPLE_SPARE",
          "model": "EXAMPLE 1TB",
          "size": 1099511627776,
          "filesystem": "ext4",
          "weakIdentity": false,
          "refused": false
        }
      ],
      "shares": [
        {
          "name": "media",
          "allocationMethod": "highwater",
          "highWater": true,
          "include": [],
          "exclude": [],
          "warningCount": 1
        },
        {
          "name": "backup",
          "allocationMethod": "fillup",
          "highWater": false,
          "include": [],
          "exclude": [
            "disk3"
          ],
          "warningCount": 0
        },
        {
          "name": "documents",
          "allocationMethod": "mostfree",
          "highWater": false,
          "include": [],
          "exclude": [],
          "warningCount": 0
        }
      ],
      "boot": {
        "mode": "usb"
      },
      "capture": {
        "state": "present",
        "capturedAt": "2026-10-03T12:51:52Z"
      }
    }
  }
};

export const migrationPendingTemplates: MigrationTemplates = {
  "counts": {
    "clean": 1,
    "withWarnings": 1,
    "failed": 0,
    "templateOnly": 0,
    "allTemplates": false,
    "composeProjects": 1
  },
  "templates": [
    {
      "name": "gateway",
      "file": "my-gateway.xml",
      "class": "running",
      "counted": true,
      "status": "warnings",
      "warningCount": 2
    },
    {
      "name": "photos",
      "file": "my-photos.xml",
      "class": "autostart",
      "counted": true,
      "status": "clean",
      "warningCount": 0,
      "autostartPosition": 1
    }
  ],
  "composeProjects": [
    {
      "name": "stack",
      "containers": [
        "stack-web"
      ],
      "status": "previewed"
    }
  ]
};
