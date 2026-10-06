---
title: Why disks wake up
description: Why a pool of separate disks can wake more of them than you expect, what Hoserva does to avoid waking disks itself, and how to read the wake events page.
---

A disk spins down when nothing has used it for a while, and it wakes up when something reads or writes it. If your disks keep waking when you expect the server to be idle, the cause is almost always something that touches the pool. This page explains why a pool is more sensitive to that than a single disk, what Hoserva does about it, and what the **Wake events** page can and cannot tell you.

## Why does a pool wake more disks?

A pool presents several disks as one set of folders, and it does not keep an index of which disk holds which file. When something asks for the contents of a folder, the pool has to ask every disk that might hold part of it. When something opens a file, the pool has to find the disk that holds it. A request that reads one file can therefore wake several disks to find it.

Every pooled setup works this way, whichever software provides it. The difference is how much help you get in finding and avoiding the cause.

The things that cause the requests are usually programs, not people:

- A media server scanning its library for new files.
- An app rescanning folders, or a backup job reading a whole share.
- A file indexer. The `updatedb` tool that Debian's `plocate` and `mlocate` packages install, for example, walks every file when it runs.
- A computer on your network that indexes or previews a share you have mounted.

## What does Hoserva do to avoid waking disks?

Hoserva does its own work in ways that leave sleeping disks alone:

- **Nothing on a timer reads your data disks, apart from the jobs you schedule.** The nightly maintenance (the mover, the sync and the scrub, at 02:00 by default) and the SMART self-tests read the disks on purpose, at the times set in **Settings → Schedules**. Nothing else does. The parity status on the dashboard comes from SnapRAID's saved state and not from a scan of your files. The list of changes since the last sync is read only when you ask for it, and Hoserva warns you first that it spins up every data disk. The **Browse** tab of a share shows **Browsing may wake disks** before you list its files.
- **Health checks leave sleeping disks alone.** When Hoserva reads a disk's SMART health data, it asks the drive not to wake it. A disk that is asleep is skipped and not woken.
- **App data can stay off the array.** A share set to **Cache only** keeps its files on the cache disk and never touches an array disk. [Cache and mover](../concepts/cache-and-mover.mdx) explains the modes.
- **A create policy can keep most disks asleep.** The **Quiet disks** policy fills one disk at a time, so new files do not spread over every disk. **Keep folders together**, the default, keeps related files on the same disk, so playing a series wakes one disk and not six. See [How pooling works](../concepts/how-pooling-works.md).
- **The pool uses the kernel's cache.** The pool is mounted so that the kernel remembers folder and file information for one second, which saves some repeated lookups.

Hoserva cannot stop a program on your server from reading the pool. If something keeps waking your disks, the fix is to change that program's behaviour. Exclude the pool paths `/mnt/user` and `/mnt/disk*` from `updatedb` and from other file indexers, and set media server and app library scans to run less often.

## How do I read the wake events page?

Open **Storage → Wake events**. The page lists the disks that have woken, with:

| Column | What it shows |
|---|---|
| **Disk** | The disk's device. |
| **Last wake** | When Hoserva last saw the disk go from asleep to active. |
| **Awake duration** | How long the disk stayed awake after that wake. |
| **Wakes today** | How many times the disk woke today. |

Hoserva notices a wake when it checks a disk's health, so the time is when it was noticed, which can be a little after the disk actually woke. A disk with no wakes does not appear. When nothing has been recorded yet, the page shows **No wake events recorded yet**.

Use the page to see a pattern. A disk that wakes at the same time every night points to a scheduled job. Wakes during the nightly maintenance and a self-test are expected. A disk that wakes many times a day points to something that touches it repeatedly. Compare the times with your schedules in **Settings → Schedules** and with the timers of your own apps.

The page does not say which program or app caused a wake. To find the cause, match the wake times against what you know runs at those times, and stop suspects one by one.

**Storage → Pool overview** shows each disk's current state, such as **Spun down** or **Active**.

## Next steps

- [How pooling works](../concepts/how-pooling-works.md)
- [Cache and mover](../concepts/cache-and-mover.mdx)
