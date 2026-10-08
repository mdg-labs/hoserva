---
title: Backing up appdata
description: Back up the settings and data your apps keep on the cache disk, restore one app from an archive with a preview, and choose where the archives go.
---

Appdata is the folder of settings, databases and other files each of your apps keeps on the cache disk. It is outside parity, so a failed cache disk loses it, and Hoserva backs it up separately. This page shows how the appdata backup works, how to choose its destinations and per-app settings, and how to restore one app.

This page covers the web UI.

## Why is appdata backed up separately?

Parity covers the data disks of your array. The cache disk is not one of them, and appdata lives there. Without its own backup, the cache disk would be a single point of failure for every app on the server. [Cache and mover](../concepts/cache-and-mover.mdx) explains the split, and [Parity is not backup](../concepts/parity-is-not-backup.md) explains what parity does and does not cover.

The appdata backup covers the apps' own files only. To back up the files in your shares, see [Backing up your data](./backing-up-your-data.mdx).

Hoserva backs up an app when one of its folders is inside the appdata folder on the cache disk. A server without a cache disk has no appdata folder, so the backup has nothing to do there.

## What happens by default?

| What | Default |
|---|---|
| Scheduled appdata backup | On, weekly: every Sunday at 04:00 |
| Snapshot before an app update | On, for an app whose appdata is on the cache disk |
| Destinations | Every enabled destination except the **Boot device** one |
| Retention | 7 daily, 4 weekly and 6 monthly archives per destination |

Each app gets its own archive, so restoring one app does not mean unpacking everything. An app is included and stopped while its files are copied, unless you change that. The backup is refused while the array is stopped.

The snapshot before an update is a backup of that one app, taken just before Hoserva updates it. If no destination holds it, the update does not go ahead.

The schedule is on **Settings → Schedules**, in the **Other recurring jobs** card under **Appdata backup**. You can change its **Frequency** and **Time** there, or turn it off.

## Choose where the archives go

Appdata archives are written to the same destinations as config backups. A fresh install has two local destinations: the boot device and a folder on the pool. Appdata archives are large and the boot device is small, so the **Boot device** destination is skipped for them.

:::warning[Both default destinations are inside your server]
**An archive on the pool does not survive a fire or a theft.** Add a destination outside the server for appdata you cannot recreate.
:::

1. Open **Settings → Backup & restore**.
2. In **Backup destinations**, select **Add destination**.
3. Enter a **Name** and choose a **Type**: **Local folder**, **SMB share**, **S3-compatible storage**, **SFTP server**, **WebDAV server** or **rclone remote**.
4. Fill in the folder or bucket and the connection details. For an NFS share, mount it on the server first and add its mount path as a local folder.
5. Set the **Retention** counts for **Daily**, **Weekly** and **Monthly** archives, then select **Add destination**.
6. Select **Test connection** on the new row. Hoserva writes a file to the destination, reads it back and removes it.

Archives written to a remote destination are always encrypted with your backup passphrase. Keep that passphrase safe: an archive cannot be opened without it.

## Choose which apps are backed up

In the **Appdata backup** section of **Settings → Backup & restore**, each app that keeps files in the appdata folder has a row with two switches:

- **Back up** includes the app in the appdata backup.
- **Stop while copying** stops the app while its files are copied and starts it again afterwards.

:::warning[Do not leave a database app running during its backup]
**A database that is running while its files are copied can give an archive that does not restore.** Hoserva marks known database images with **Database image**. Leave **Stop while copying** on for them. A known database image that is included but not stopped is named in a warning line in the backup's job output.
:::

Hoserva recognises common database images such as PostgreSQL, MariaDB, MySQL, MongoDB and Redis. A file-based database in an image it does not recognise is not marked, which is why every app is stopped by default.

The app is down only for the copy. Archives are written to the destinations after it has started again, so a slow upload does not extend the downtime.

## Back up now

Select **Back up now** at the top of the **Appdata backup** section to back up every included app. The **Back up now** button in an app's row backs up that app alone, and works for included apps only. The backup runs as a job, and you can follow it in **Jobs**.

## Restore one app

Restoring replaces an app's appdata with the contents of an archive. Hoserva shows what would change before it changes anything, and takes a snapshot of the current appdata first.

Hoserva restores only an archive it can show this server wrote, and only into the folders the app mounts now. The archives Hoserva writes carry a tag that only this server can produce, and a restore refuses an archive whose tag is wrong. An older archive without a tag is restored only from an encrypted destination (see Troubleshooting below). If the folders an archive names are not the app's folders today, or the app no longer exists, the restore is refused before it stops anything.

1. Open **Settings → Backup & restore**.
2. In **Appdata archives and restore**, find the archive. They are listed newest first, with their destination, time and size. Select **Refresh archives** if the list is out of date.
3. Select **Restore** on the archive's row. Hoserva starts a preview and shows the files that would be **Replaced**, **Added** and **Removed**, with counts and sizes. A preview changes nothing and stops no app.
4. Read the preview. If the archive and the current files are identical, it says nothing would change.
5. Type the app's name to confirm, then select **Restore**.

:::danger[A restore replaces the app's appdata as a whole]
**Each folder in the archive is replaced, and files that are not in the archive are removed.** The preview lists what would go. Check it before you confirm.
:::

When the restore runs:

- A snapshot of the current appdata is written to your destinations first. If no destination holds it, the restore does not start and nothing is changed. It appears in the archive list with the **Snapshot before a restore** label, so you can undo a restore with another restore.
- The app is stopped for the restore and started again afterwards. Other apps that mount the same folders are stopped too.
- The restore runs as a job, and it cannot be cancelled.

## How long are archives kept?

Each destination has its own **Retention**: how many of the newest daily, weekly and monthly archives to keep. Only archives this Hoserva wrote are ever removed. A lower count takes effect at the next backup. You can change retention on a destination's row with **Edit**.

Snapshots taken before a restore or an update are kept apart from this: the five newest per app survive, and they never take the place of a regular backup. Removing a destination leaves the archives already on it where they are.

## Troubleshooting

### The appdata section lists no apps

No app keeps files in the appdata folder yet, or Hoserva cannot reach the Docker Engine. The section says which. Apps you install appear here once they use the appdata folder.

### The backup does not start because the array is stopped

Appdata cannot be backed up or restored while the array is stopped. Start the array and run the backup again.

### A restore is refused because the archive "predates archive authentication"

Archives written by an older Hoserva have no tag. Hoserva still restores them from an encrypted destination: every remote destination, and a local folder with encryption turned on. It refuses them from an unencrypted local folder, such as the default pool folder, because anyone who can write to that folder could have left a file there.

1. Run **Back up now** for the app so a new archive carries the tag.
2. To get data out of the old archive, unpack it by hand into an empty folder you own. Do this only if you trust the archive. Hoserva writes archives so that only root can read them, so read the file with `sudo` and run `tar` itself as your normal user:

   ```bash
   sudo cat <archive> | tar --zstd -xf - -C <folder>
   ```

   :::warning[Unpack as your normal user, not as root]
   **Do not put `sudo` in front of `tar`.** As root, `tar` gives each file the owner the archive names and keeps its setuid and setgid bits, which let a program run with another user's rights. As a normal user, the files are yours and those bits are dropped. To check an unpacked folder for files that still carry them, run `find <folder> -perm /6000`.
   :::

3. The `dirs` list in the `hoserva-appdata.json` file in that folder names the app's folders in order. Stop the app. The files of the first folder in the list are in `data/0`, those of the second in `data/1`, and so on. For each folder, keep a copy of the current one, copy the unpacked files in, and give them the owner of the app's folder, because the files you unpacked are owned by you and the app needs its own user to read them:

   ```bash
   sudo cp -a <app folder> <app folder>.before
   sudo cp -R --preserve=timestamps <folder>/data/0/. <app folder>/
   sudo chown -R --reference=<app folder>.before <app folder>
   ```

   This gives every restored file the owner of the app's folder. If the app keeps some of its files under another owner, compare with `<app folder>.before`, which keeps the original owners, and check the app's own documentation. The `.before` copy is also the way back if the result is wrong.

The snapshot before an app update is an archive too. A snapshot taken by an older Hoserva and kept in an unencrypted folder cannot be used by **Revert**.

### A restore is refused because a folder is not the app's folder

An archive can be restored only into the folders the app has mounted now. This happens when you changed the app's folders after the backup, or when the app was deleted. Install the app again with the same folders, then restore.

### A destination could not be listed

The archive list names the destination it could not read. Check its status on **Backup destinations** and use **Test connection**. The archives on the other destinations are still listed.

## Next steps

- [Cache and mover](../concepts/cache-and-mover.mdx)
- [Parity is not backup](../concepts/parity-is-not-backup.md)
- [How parity works](../concepts/how-parity-works.mdx)
