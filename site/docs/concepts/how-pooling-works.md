---
title: How pooling works
description: How Hoserva combines disks of different sizes into one set of shared folders, where new files are placed, and what changes when a disk is missing.
---

Pooling combines several independent disks into one view, so you see one set of folders instead of one per disk. Hoserva uses mergerfs for this. You need to understand it to choose where new files go and to know what a single disk failure costs you.

Pooling gives you:

- **Disks of any size.** A 4 TB, an 8 TB and a 16 TB disk all count in full towards the pool.
- **Instant capacity.** Adding a disk makes its space available at once, with no rebuild.
- **Contained failures.** Every file lives whole on one disk, so a disk that cannot be recovered takes only its own files with it.
- **Ordinary disks.** Each data disk is a plain filesystem you can read on its own.

## The everyday case

Each data disk is mounted on its own, at `/mnt/disk1`, `/mnt/disk2` and so on. The pool presents them together under `/mnt/user`. A folder such as `/mnt/user/media` shows the files of every disk that holds something in `media`, merged into one listing.

Every share gets its own mount at `/mnt/user/<share name>`. A share is a named folder in the pool with its own cache mode, create policy, and SMB and NFS settings. The mount for a share can differ from the others in which disks it uses and how it places new files, which is what lets one share be cache-only and another array-only. See [Cache and mover](../concepts/cache-and-mover.mdx).

Samba, NFS and your apps use the pool paths, so they do not need to know which disk holds a file.

## Where do new files go?

When you create a file, the pool picks one disk for it. The rule it follows is the share's create policy. Hoserva labels the four policies in plain language, with the technical name in brackets:

| Label | Technical name | What happens to a new file |
|---|---|---|
| **Keep folders together** | `mspmfs` | Goes to a disk that already holds this folder. If that disk has no room, the pool tries the parent folder's disk, then its parent's. |
| **Balance across disks** | `mfs` | Goes to the disk with the most free space. |
| **Quiet disks** | `lfs` | Goes to the disk with the least free space that still fits, so disks fill one at a time and the rest can stay asleep. |
| **Fill disks in order** | `ff` | Goes to the first disk with room, always in the same disk order. |

**Keep folders together** is the default for a new share. It keeps a TV series on one disk instead of spreading its episodes over six, which limits how many folders a single failed disk can break and lets the other disks stay asleep while you watch.

Whichever policy you choose, the pool does not place a new file on a disk that has less free space than a floor, 50 GB by default. This keeps room free on every disk for parity and for files that grow.

## One file is read from one disk

Because a file is stored whole on one disk, a single transfer runs at the speed of that one disk. A large copy tops out around what one spinning disk delivers, which is well below a striped RAID array built from the same disks. No setting changes this, because the limit comes from how pooling works.

Several transfers at the same time do spread across disks. Several people streaming, or several apps writing at once, each use the disk that holds their file. A single big transfer does not get faster by adding disks.

## What happens when a disk is missing?

The pool keeps working from the disks that remain. The files on the missing disk do not appear in the listing, and new files go to the other disks. Nothing is rebuilt and no other disk is touched.

If a disk is missing when the server starts, the pool still mounts from the remaining disks. Shares, apps and VMs wait until you acknowledge the degraded state, so that nothing starts against a pool that is missing part of its data without your decision. [What happens when a disk dies](../concepts/what-happens-when-a-disk-dies.md) describes this and the recovery in detail.

## Where to see the pool

- **Web UI:** open **Storage → Pool overview**. It shows the pool's capacity, whether it is mounted, and each disk with its state, such as **Active**, **Spun down** or **Missing — not detected**.
- **CLI:** run `hoserva pool status` for the same per-disk breakdown.
- **Per-disk distribution:** open a share in **Shares** and look at its **General** tab. After the share's first parity sync it lists how much of the share each disk holds.
- **Create policy:** in **Shares**, open a share and select its **Allocation** tab to choose a policy. `hoserva share create NAME --create-policy mspmfs` sets it when you create the share.

## Next steps

- [How parity works](../concepts/how-parity-works.mdx)
- [Cache and mover](../concepts/cache-and-mover.mdx)
- [Why disks wake up](../concepts/why-disks-wake-up.md)
