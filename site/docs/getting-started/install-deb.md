---
title: Install on Debian
description: Download and verify the Hoserva .deb package, install it on Debian 13 with apt, sign in over HTTPS on port 8008 and check the installation with hoserva doctor.
---

Hoserva is installed on an existing Debian system from a `.deb` package. The package installs the Hoserva daemon, the `hoserva` command-line tool and the Debian packages Hoserva drives. After it is installed you finish the setup in a web browser.

## Before you start

- Debian 13 (trixie) installed on the server's boot device. See [Requirements](./requirements.md) for the boot device and disk layout.
- Root access, or a user that can run `sudo`.
- A connection to Debian's package sources, because `apt` installs Hoserva's dependencies from them.
- A computer on the same local network with a web browser.

Installing the package does not format, mount or change any of your data disks. You choose the disks later, in the setup wizard.

## Download and verify the package

Releases are published on the project's [release page](https://github.com/mdg-labs/hoserva/releases). Each release has one `.deb` package per architecture, a `SHA256SUMS` file that lists the checksum of both packages, and a `SHA256SUMS.sig` file that is a detached signature of that list. Versions marked as pre-release on the page are beta versions.

1. On the release you want, download the package that matches your processor, for example `hoserva_<version>_amd64.deb`, and the `SHA256SUMS` file. Save both in the same folder on the server.
2. In that folder, check the package against its checksum.

   ```bash
   sha256sum --ignore-missing -c SHA256SUMS
   ```

   The package's line must end in `OK`. If it says `FAILED`, delete the file and download it again.

The checksum shows that the download is complete and unchanged. The `SHA256SUMS.sig` file lets the signature on the list be checked against the project's release-signing key. Hoserva itself does that check whenever it installs an update later.

## Install the package

1. Install it with `apt`. Keep the `./` in front of the file name: it tells `apt` that this is a local file.

   ```bash
   sudo apt install ./hoserva_<version>_amd64.deb
   ```

2. `apt` lists the extra packages it needs. Confirm the list and let it finish.
3. Check that the service is running.

   ```bash
   systemctl status hoserva
   ```

   The service should be listed as `active (running)`.

## What does the package set up?

| What | Details |
|---|---|
| The daemon | `hoservad` runs as the `hoserva.service` systemd unit and starts at boot. It runs as root, because it formats disks and manages mounts. |
| The command-line tool | `hoserva`, installed in `/usr/bin`. |
| Debian packages | `mergerfs` and `snapraid`, which Hoserva drives, plus `smartmontools`, `hdparm`, Samba and the NFS server. `rclone`, `unattended-upgrades` and `nut` are installed as recommended extras. |
| The `hoserva` group | A group with no members. Anyone you add to it can use the `hoserva` command on this server without signing in, so treat membership as root access. |
| The `hoserva-apps` user | A system user with ID 99, created only if that ID is free. Apps use it so that files keep the same owner after a migration. If the ID is taken, the install says so and creates no user. |
| The SMART background service | Debian's own `smartd` service is switched off. It would wake your disks on its own schedule. Hoserva reads SMART data and runs self-tests on the schedule you set. |
| A disk-arrival rule | When a disk with a filesystem appears, Hoserva re-checks whether the array can start. |
| Update settings | A configuration for `unattended-upgrades` that limits automatic updates to Debian security updates and turns off automatic reboots. |

The package creates no login account for people, no default password and no array. Docker is not installed. You need it only for apps, and you install it from Docker's own repository.

## Sign in for the first time

Hoserva's web interface listens on port 8008 and uses HTTPS only. A request over plain HTTP gets a short message asking you to use `https://`.

1. On the server, find its address.

   ```bash
   hostname -I
   ```

2. On a computer on the same network, open `https://<server-address>:8008` in a browser.
3. The browser warns that the connection is not private. Hoserva created its own certificate when it first started, and no browser trusts it yet. The connection is encrypted. The warning is about who issued the certificate. Continue to the page. In most browsers that means choosing **Advanced** and then proceeding to the site.
4. The **Welcome** wizard opens, because no account exists yet.

By default Hoserva accepts connections only from the server itself and from local-network addresses (private ranges, link-local addresses and Tailscale's `100.64.0.0/10` range). A connection from the internet is refused.

To get rid of the warning later, open **Settings → Network**. The **HTTPS** card can issue a Let's Encrypt certificate for a domain you own (**Set up Let's Encrypt**), or you can accept the self-signed certificate.

### The Welcome wizard

The wizard has four steps.

1. **Create the admin account.** Choose a **Username** and a **Password** of at least 12 characters. There are no default credentials. The checkbox **Set up an authenticator app (recommended)** adds a second sign-in step.
2. **System check.** Hoserva runs the same checks as `hoserva doctor`. You cannot continue while a storage dependency check has failed. If the server already has Samba shares, NFS exports, fstab mounts or Docker containers, Hoserva lists them under **Existing host configuration**. It never overwrites them. For each item you choose **Import into Hoserva** or **Leave unmanaged**.
3. **Basics.** Set the **Hostname** and the **Timezone**. The timezone matters because the nightly maintenance starts at 02:00 in it. You can add a **Notification channel** (Email, Gotify, ntfy, Discord or Webhook) and a **Backup passphrase**. If you skip the channel, Hoserva warns that you will not be told about disk failures. If you skip the passphrase, it warns that a restored backup does not restore the secrets the passphrase protects.
4. **Where next?** Choose **Fresh install** to continue to the storage setup. Choose **Migrating from Unraid** if you are bringing an existing array from another system, which opens the migration tool; its guide starts at the [migration overview](../migrating-from-unraid/overview.mdx). Select **Finish setup**.

The next page is the storage setup wizard. Continue with [Create your first array](./first-array.mdx).

## Check the installation with hoserva doctor

`hoserva doctor` runs the same checks as the wizard's system check. Run it as root or as a member of the `hoserva` group.

```bash
sudo hoserva doctor
```

Each line starts with the result of one check, either `pass`, `warn` or `fail`, in square brackets.

```text title="Output (shortened)"
[pass] Disk inventory: 4 data disk(s) visible (5 total block devices)
[warn] Docker Engine: Docker is not installed — Apps will not be available
[warn] Pool mount: The storage pool is not mounted at /mnt/user
```

On a new installation it is normal to see warnings about Docker, the pool mount and parity. Docker matters only if you want apps, and the pool and parity do not exist until you create the array. A `fail` result needs attention before you continue, and the command then exits with status 4.

## Troubleshooting

### The browser cannot reach the page

1. Check the service on the server with `systemctl status hoserva`. If it is not running, `journalctl -u hoserva` shows why.
2. Check that the address and the `https://` prefix are right, and that the port is 8008.
3. Check that your computer is on the same local network as the server. A connection from outside it is refused by default.

### The installation stops with an error about missing packages

`apt` could not find `mergerfs`, `snapraid` or another dependency in Debian's package sources. Check that the server is connected to the network, run `sudo apt update`, and install the package again.

### The system check fails

Open the failed check in the wizard, or read its line in the `hoserva doctor` output. A failed storage dependency check usually means that `mergerfs` or `snapraid` is missing or too old. Reinstall the Hoserva package with `apt` so that its dependencies are installed again.

## Next steps

- [Create your first array](./first-array.mdx)
- [Requirements](./requirements.md)
