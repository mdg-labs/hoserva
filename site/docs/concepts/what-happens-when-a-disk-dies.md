---
title: What happens when a disk dies
description: What a failed disk costs you, what keeps working, what parity can and cannot rebuild, and where to start recovering.
---

When a data disk fails, the rest of the pool keeps working and you can rebuild the lost disk from parity. What you get back depends on the last time parity was updated. This page explains what happens and what to expect before you start a recovery.

## What can be rebuilt?

Parity can rebuild only the data that was on the disk at the last successful sync. Files you wrote or changed after that sync are not covered, and they are gone with the disk. A file you deleted after the sync, on the other hand, comes back.

:::warning[Check when parity last synced]
**A rebuild restores the failed disk as it was at the last successful sync.** If your last sync ran at 02:00 and the disk failed at 20:00, everything written to that disk during the day is lost. Look at the time of the last sync on **Storage → Parity** before you decide what to expect.
:::

Changes to files on your other disks since the last sync can also make some blocks impossible to rebuild, when those files share their parity positions with the lost data. A fix that leaves blocks it could not rebuild ends as failed, and its error says how many.

To see which files changed since the last sync, select **Run diff** under **Changes since last sync** on **Storage → Parity**. It wakes every data disk to compare the files against parity, so run it when you need the answer. The **Guided recovery** on the same page has a step called **What cannot be recovered**, but it shows only a warning that files written after the last successful sync are not on parity. It does not list them. [How parity works](../concepts/how-parity-works.mdx) explains why parity is updated on a schedule.

## What keeps working?

- **The other disks.** The pool keeps serving the files on every disk that is still present. You do not lose the rest of your data, because each file lives whole on one disk.
- **Only the failed disk's files are at risk.** A file is rebuilt or lost depending on whether it was on that disk and covered by the last sync.
- **A failed parity disk loses no data.** Your files are intact and readable. You have no protection against a second failure until the parity disk is replaced and parity is rebuilt.

The failed disk's files do not appear in the pool until you replace the disk and rebuild it.

## How do I see that a disk failed?

Hoserva shows the degraded state in several places:

- The top bar shows **Degraded**, and the **Array degraded** banner appears with a **View disks** action.
- On **Storage → Pool overview**, the disk shows a state such as **Missing — not detected** or **Failed**.
- `hoserva pool status` lists the disks and their states.

## What happens if a disk is missing when the server starts?

The pool mounts from the disks that are present. Shares, apps and VMs do not start by themselves, so that nothing runs against a pool that is missing part of its data without your say-so. Hoserva shows a banner with **Acknowledge and start services**. Select it when you are ready to run without the missing disk. The `hoserva array acknowledge-degraded` command does the same.

After you acknowledge, the banner changes to **Array degraded — running acknowledged** and stays until you replace the disk and parity has been synced.

## What if more than one disk fails?

| What fails | What happens |
|---|---|
| One data disk | The pool keeps serving the rest. You rebuild the disk from parity. |
| Two data disks, with one parity disk | Parity can rebuild only one of them. The other disk's files are lost, unless they exist elsewhere. With two parity disks, both can be rebuilt. |
| A parity disk | Your data is intact and fully readable. You replace the parity disk and rebuild parity. |
| A disk that drops out unexpectedly, for example a loose cable | A disk that suddenly holds no files makes the threshold guard hold the next sync, so the parity from before the failure is kept. |
| The cache disk | Files waiting to be moved and all **Cache only** data are lost. The array is intact. Restore app data from your backup. |
| The boot device | The files on your array are untouched. Reinstall Debian and Hoserva, then restore your settings from a config backup. |

## What do I do after a disk fails?

You replace the failed disk with **Replace disk** on **Storage → Pool overview**. It formats the new disk and rebuilds the failed disk's contents onto it from parity. Remember that the rebuild restores the disk only as it was at the last successful sync.

A step-by-step guide for swapping the disk is not published yet. Until then, do not remove a disk from a running array, and do not start a recovery before you have checked when parity last synced.

If you need to get back a single file that was deleted by mistake, and not a whole disk, use `hoserva fix --confirm --path /mnt/user/<share>/<file>`. It restores that one file and leaves every other change since the last sync alone.

:::danger[Guided recovery restores the whole array]
**Do not use the Guided recovery to replace a failed disk.** The **Guided recovery** on **Storage → Parity** always starts a fix of the whole array. It rewrites every file that changed or was deleted since the last sync, not only the failed disk's files, so changes you made since that sync are overwritten. To replace a failed disk, use **Replace disk**. For a single file, use the command above.
:::

## Next steps

- [How parity works](../concepts/how-parity-works.mdx)
- [Parity is not backup](../concepts/parity-is-not-backup.md)
- [How pooling works](../concepts/how-pooling-works.md)
