---
title: Parity is not backup
description: What parity protects against and what it does not, and what Hoserva backs up for you compared with what you need to back up yourself.
---

Parity protects you from a disk dying. It does not protect you from deleting the wrong folder, from ransomware, from corruption, or from a fire or a theft. Parity is redundancy, not backup. For anything you cannot replace, keep a copy somewhere else.

Read this page before you decide what you can afford to lose.

## What is the difference?

- **Redundancy** keeps the server running and your data intact when a disk fails. Parity provides it.
- **A backup** is a separate copy that survives whatever happens to the original, including your own mistakes. Parity is not one.

Parity lives in the same server as your data, and it is updated by a schedule to match the data. It records what your files look like. It does not keep a history of earlier versions.

## What does parity protect against?

| What happens | What parity does |
|---|---|
| One data disk fails | **Protects you.** The disk is rebuilt from parity and the other disks, as it was at the last successful sync. Files written after that sync are lost. |
| Two data disks fail at once, with one parity disk | **Does not protect you.** Parity can rebuild one disk. With a second parity disk it can rebuild two. |
| A parity disk fails | **Your data is untouched.** You have no protection until the parity disk is replaced and parity is rebuilt. |
| You delete a file by mistake | **Only until the next sync.** Before it, you can restore the file from parity. After it, the deletion is part of parity and the file is gone. |
| Ransomware encrypts your files | **Only if the threshold guard stops the next sync.** A change that large makes the guard hold the sync, which keeps the parity from before the attack. If the encrypted files slip past the guard, the next sync records them. |
| A file is silently corrupted | **Partly.** A scrub finds files whose data no longer matches what parity recorded, and a fix can repair them. A file that was already corrupt when it was synced is protected only in its corrupt form. |
| A fire, a flood or a theft | **Does not protect you.** The parity disk is in the same box as your data. |
| Cache-only data, or the cache disk, is lost | **Does not protect you.** The cache is outside parity. See [Cache and mover](../concepts/cache-and-mover.mdx). |

The sync timing in the "delete a file" row follows from how parity is updated. [How parity works](../concepts/how-parity-works.mdx) explains it, and why a sync never runs past a mass deletion until you decide. [Recovering files](../guides/recovering-files.mdx) shows how to get a deleted file back before the next sync.

## What does Hoserva back up for you?

Hoserva backs up its own state, so that a failed boot device does not also cost you your setup. It does not back up the files in your shares.

| What | Where it is configured | Default |
|---|---|---|
| **Config backup**: your shares, users, schedules, backup destinations and other Hoserva settings | **Settings → Backup & restore**, with the schedule under **Settings → Schedules** | Nightly, after the parity sync, to two local destinations: the boot device and a folder on the pool |
| **Appdata backup**: each app's own files, so an app can be put back as it was | **Settings → Backup & restore** | Weekly |

You can run the appdata backup now with **Back up now**, as [Backing up appdata](../guides/backing-up-appdata.md) describes, and write a config backup now with `hoserva backup run config`.

Both defaults write to disks inside your server. A destination you add under **Settings → Backup & restore** can be an SFTP server, S3-compatible storage, a WebDAV server or an rclone remote, which gets a copy out of the building.

## How do I back up my files?

Hoserva does not copy the files in your shares for you, and it does not try to replace tools built for that. The curated catalog includes backup apps: Duplicati, Backrest, Kopia, borgmatic and rclone. Open **Apps → Catalog** to find them, and install the one that fits where you want your copies to go. [Backing up your data](../guides/backing-up-your-data.mdx) walks through choosing an app, sending a copy off the server and testing a restore.

Whichever tool you use, a backup you have never restored from is a guess. Restore a few files from it once, before you need it.

## Next steps

- [How parity works](../concepts/how-parity-works.mdx)
- [What happens when a disk dies](../concepts/what-happens-when-a-disk-dies.md)
- [Recovering files](../guides/recovering-files.mdx)
- [Backing up appdata](../guides/backing-up-appdata.md)
- [Backing up your data](../guides/backing-up-your-data.mdx)
