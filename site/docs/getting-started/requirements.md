---
title: Requirements
description: The operating system, boot device and disks you need before you install Hoserva, including how many parity disks to use and when a cache SSD helps.
---

Hoserva runs on a Debian server that has its own boot device and a set of separate disks for your data. Check this page before you install, because the disk layout is easiest to get right at the start.

## What does the server need?

| Item | Requirement |
|---|---|
| Operating system | Debian 13 (trixie). Other Debian releases and Ubuntu are not tested. |
| Processor | amd64. The release also includes an arm64 package, but arm64 is not yet a supported platform. |
| Boot device | A disk of its own that holds Debian and Hoserva. It is never part of the array. |
| Data disks | At least one data disk and one parity disk, plus enough further disks for the layout rules below. |
| Network | A connection to your LAN. You manage the server from a browser on the same network. |

Debian's own package sources must be reachable during the install. Hoserva depends on `mergerfs` and `snapraid`, and Debian 13 provides both.

## Why does Hoserva need a separate boot device?

Hoserva runs on a normal Debian installation, which writes to its disk all day for logs and databases. Hoserva stores its own database, job logs, local backups and the first copy of SnapRAID's list of what is on each disk on that disk.

- **Recommended:** a small SATA SSD of its own. 128 GB is plenty. A boot SSD can be replaced without touching the data or cache disks.
- **Acceptable:** one NVMe drive split in two. In the Debian installer's partitioner, give Debian a root partition of about 60 GB, then create a second partition from the space that is left. Choose the type "Linux filesystem", set no filesystem, no mount point and no swap, and leave it unused. Hoserva can later format that spare partition as the cache. It never changes the boot disk's partition table.
- **Not supported:** a USB stick as the Debian disk. A stick wears out within months under the constant small writes.

Hoserva marks the disk Debian runs from as the boot disk and does not offer it as a data or parity disk.

## Which disks can the array use?

The array uses whole disks, with one exception: a spare partition of the boot disk can be the cache. Each one is given one of these roles when you [create your first array](./first-array.mdx):

- **Data:** holds your files. Data disks can be any mix of sizes and brands.
- **Parity:** holds the information needed to rebuild a failed data disk. See [How parity works](../concepts/how-parity-works.mdx).
- **Cache:** an optional fast disk for new writes and app data. See [Cache and mover](../concepts/cache-and-mover.mdx).

The setup wizard also lets you leave a disk out of the array with the **Ignore** role.

A disk in a USB enclosure that hides the disk's serial number is marked **Weak identity**. It can be a data disk, but it cannot be a parity disk, because Hoserva could not reliably tell it apart from another disk.

## How many parity disks, and how big?

Use one parity disk or two. Each parity disk must be at least as large as your largest data disk, because it holds one file about that size. Parity disks add no capacity, and Hoserva always formats them.

| Parity disks | Disks that can fail without losing data | Choose it when |
|---|---|---|
| 1 | Any 1 disk, data or parity | Most home arrays. The failed disk is rebuilt from parity onto its replacement. |
| 2 | Any 2 disks, data or parity | You want protection while a replacement rebuilds, or your array is large enough that a second failure during a long rebuild worries you. It costs one more disk. |

A worked example: with data disks of 4 TB, 8 TB and 12 TB, every parity disk must be at least 12 TB. The array offers 24 TB of space, and one parity disk of 12 TB or larger protects it against one failed disk.

Parity protects against a failed disk and not against deletion or a bad file. Keep a separate backup of anything you cannot replace. See [Parity is not backup](../concepts/parity-is-not-backup.md).

### How many disks does the layout need in total?

Hoserva keeps copies of SnapRAID's list of what is on each disk (its content file) on separate physical devices. Without these copies parity is useless, so Hoserva refuses a layout that cannot place enough of them. It puts one copy on the boot device and, if there is one, one on a cache disk, and the rest on data disks. The number of copies is the number of parity disks plus two.

| Parity disks | Smallest layout that works |
|---|---|
| 1 | Two data disks, plus the boot device. Alternatively one data disk and a cache that is a separate disk. |
| 2 | Three data disks, plus the boot device. Alternatively two data disks and a cache that is a separate disk. |

A cache on a spare partition of the boot disk does not count as a separate device.

## Do I need a cache SSD?

No. A cache is optional, and you can add one later. It helps when you want new writes to land on a fast disk first, or when you want your apps' data on an SSD. You can assign at most one cache disk. If you have a single NVMe drive, the split layout above lets one drive serve as both the boot device and the cache.

## Is anything else required?

Docker Engine is needed only for the Apps section, and you install it yourself from Docker's own repository. Hoserva reports whether Docker is present in its system check, and everything else works without it.

## Next steps

- [Install on Debian](./install-deb.md)
- [How pooling works](../concepts/how-pooling-works.md)
