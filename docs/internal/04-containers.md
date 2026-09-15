# Hoserva — Container Management and Templates

---

## 1. Decision

**Build container management, but deliberately narrow. Not a Portainer clone.**

### Why build it

A guided app catalog is what makes a home server usable day to day, and the way most homelab users run their services. Anyone migrating an existing setup expects: pick a template → review paths and ports → running.

Deferring this ("just install Portainer") breaks the migration at exactly the point where it hurts, and reduces Hoserva to a storage manager rather than a complete home server.

### Why narrow

A generic Docker manager — networks, volumes, registries, Swarm, image layer inspection — is its own product with a large surface area and good existing implementations. Building it badly is worse than not building it.

The dividing line: **Hoserva owns the path from "I want Jellyfin" to "Jellyfin is running against my media share."** Everything beyond that is Portainer's job.

### In scope

- App catalog with curated and imported templates
- Template → Compose generation with pool-aware path defaults
- Guided install with port conflict detection and share-aware path picking
- Lifecycle: start, stop, restart, update, recreate, remove
- Logs, stats, health status
- Update detection and bulk update
- Compose import and raw editing
- Unraid XML template conversion
- Privilege warnings on templates requesting elevated access

### Out of scope

- Network editor beyond bridge / host / macvlan selection
- Volume manager — volumes point at pool paths, that is the design
- Registry management, image layer inspection
- Swarm, Kubernetes, multi-host
- Building images

---

## 2. Coexistence

Stacks are plain Compose files:

```
/var/lib/hoserva/stacks/<name>/docker-compose.yml
/var/lib/hoserva/stacks/<name>/.env
/var/lib/hoserva/stacks/<name>/meta.json     # Hoserva metadata: template source, id and revision, install time
```

Driven through the Docker Engine API and `docker compose`. No proprietary format, no exclusive claim on the daemon.

Consequences, all intentional:

- Portainer, Dockge, or Dokploy can run alongside and manage the same containers
- Containers created outside Hoserva appear as **unmanaged** — visible, lifecycle actions available, untouched by the template system
- A user can `cd` into a stack directory and run `docker compose` by hand
- Uninstalling Hoserva leaves every container running and every Compose file intact — including `dpkg --purge`: the package's purge script removes Hoserva's own state but never `/var/lib/hoserva/stacks/`

**The user is never locked in.** This should be stated plainly in user-facing docs: lock-in is a reasonable concern with any appliance-style platform, and here the answer is simply no.

---

## 3. Prerequisite handling

Docker Engine and the Compose plugin must be present. The `.deb` does not install them (decision D8) — Docker's own repository is the correct source, and pulling it in as a dependency creates version conflicts on systems that already have it.

Behaviour:

- `postinst` checks and prints install instructions if missing
- `hoservad` starts regardless, but the Apps section shows a clear prerequisite banner with the exact commands
- `hoserva doctor` reports version and API reachability
- The ISO bundle (doc 07 §1, Phase 4) ships both preinstalled

Version policy (Q38): negotiate the Docker Engine API version at runtime rather than pinning an Engine release number; require the Compose v2 plugin; `hoserva doctor` warns when the installed Engine is a release upstream no longer supports. Documentation points to Docker's own apt repository.

**Storage backend: a plain directory, never a loopback image (Q62).** Docker's data-root points at a directory on cache (`/mnt/cache/docker`), using the Engine's standard `overlay2` driver. Hoserva never creates a fixed-size loopback image for Docker's own storage — that construction is Unraid-specific, and a full loopback image needing a manual resize is one of its most common support complaints. A plain directory has no size of its own to run out of; it is sized by the cache device, which already has its own capacity monitoring and alerting (doc 02 §3). The move is offered, never applied silently: with no cache disk, or when Docker already holds containers or images, the data-root stays at `/var/lib/docker` (Q76). Docker starts only once storage is up, through a managed systemd drop-in (Q69).

---

## 4. Catalog sources

Hoserva's catalog is its own (D19): the curated template repository in §7 is the only built-in source. Beyond it, a user can add a catalog source URL of their own — a catalog archive in Hoserva's format (§7) — which Hoserva fetches only because the user added it and badges as user-added.

**Design implication:** catalog sources sit behind one interface, so a new source changes a config default, not the architecture.

---

## 5. Unraid XML template converter

### Source

Per-container XML files under `/boot/config/plugins/dockerMan/templates-user/` on an Unraid flash drive.

### Field mapping

| Unraid XML | Compose | Notes |
|---|---|---|
| `<Repository>` | `image` | Direct |
| `<Name>` | service name / `container_name` | Sanitise to a valid Compose key |
| `<Config Type="Port">` | `ports` | `HostPort:ContainerPort/protocol` |
| `<Config Type="Path">` | `volumes` | Host path from `<Value>`, container path from `Target`, plus `Mode` (rw/ro) |
| `<Config Type="Variable">` | `environment` | Name, default value, description retained for the install form |
| `<Config Type="Device">` | `devices` | GPU and tuner passthrough |
| `<Config Type="Label">` | `labels` | |
| `<Network>` | `network_mode` | `bridge`, `host`, or a custom network name |
| `<Privileged>` | `privileged` | Flagged in the privilege summary |
| `<ExtraParams>` | various | **The hard one** — see below |
| `<PostArgs>` | `command` | |
| `<CPUset>` | `cpuset` | |
| `<Shell>` | — | Dropped, Unraid-specific |
| `<WebUI>` | metadata | Used for the clickable link on the container card |
| `<Icon>` | metadata | Cached locally; do not hotlink |
| `<Overview>`, `<Category>`, `<Support>`, `<Project>` | metadata | Catalog display |
| `<Requires>` | metadata | Shown as a prerequisite note in the install flow |
| `<DonateLink>` | metadata | Shown on the app detail page — the maintainers deserve it |

### Path handling

Because the pool is at `/mnt/user` and cache at `/mnt/cache` (decision D10), **paths map identically with no rewriting.** Paths pointing anywhere else are flagged for manual review rather than silently translated:

- `/boot`, `/mnt/disks/` (Unassigned Devices), and other Unraid-specific mounts
- `/mnt/user0` — Unraid's array-only view of shares, which has no Hoserva equivalent path
- `/mnt/<pool>/` for Unraid 6.9+ named pools other than the one mapped to `/mnt/cache` (doc 05 §2)

### Ownership variables

Unraid templates conventionally pass `PUID=99` / `PGID=100` (`nobody:users` on Unraid). Hoserva keeps those values working unchanged: GID 100 is `users` on Debian too, and a `hoserva-apps` system user is pinned to UID 99 (Q26). The converter carries the values through as-is.

### `<ExtraParams>` — the messy field

Arbitrary `docker run` flags as a raw string. Approach:

1. **Parse** with a `docker run` flag parser, not a regex
2. **Translate known flags** to Compose equivalents: `--restart`, `--memory`, `--cpus`, `--device`, `--cap-add`, `--cap-drop`, `--security-opt`, `--sysctl`, `--ulimit`, `--dns`, `--hostname`, `--tmpfs`, `--shm-size`, `--runtime`, `--gpus`, `--log-opt`
3. **Never silently drop.** Unknown flags are emitted as a comment in the generated Compose file *and* surfaced as a warning in the install preview:
   ```yaml
   # Hoserva: could not translate the following Unraid ExtraParams:
   #   --some-exotic-flag=value
   # Review and add the Compose equivalent manually if required.
   ```
4. **Never interpolate into a shell.** These strings come from third parties; they are parsed into structured Compose fields, never concatenated into a command line.

### Other known-hard cases

- **macvlan / custom networks** — require a pre-existing equivalent network. The converter detects the reference and flags it with the exact `docker network create` command, rather than guessing or creating networks silently; v1 has no network-creation UI (Q37).
- **Unraid-specific variables** like `$$` substitutions and `HOST_OS` are recognised and handled or flagged.
- **Multi-container templates** (the "AIO" pattern, bundling app + database + worker) map naturally to multi-service Compose, which expresses them directly.
- **`:latest` tags** — carried through as-is, but flagged in the UI as a reproducibility risk, since a portion of the ecosystem pins nothing.

### Output is always reviewable

Generated Compose is shown side by side with the source XML before anything runs, with all warnings listed. Never a silent conversion followed by a container that behaves subtly differently.

---

## 6. Update handling

- Check registries at most once a day, with jitter, by requesting only each image's manifest — never pulling; optional per-registry credentials stored as secrets; a rate-limited registry is skipped until the next day, and the UI says so (Q81)
- Distinguish **digest changed on the same tag** (the common `:latest` case) from **a genuinely new version tag**
- Show both, labelled differently — "new build of `latest`" is not the same as "2.1 → 2.2"
- Bulk update with per-container opt-out
- **Pre-update appdata snapshot** for containers whose appdata sits on the cache, so a bad update is recoverable. A bad container update is one of the most common ways a working homelab service breaks.
- Rollback: keep the previous image locally for a configurable period and offer a one-click revert

---

## 7. Curated catalog

Hoserva's catalog is its own template repository (D19) — `templates/` in the monorepo until the first external template PR, then split out (doc 12 §7, Q39). It is the only built-in catalog source.

### What goes in it

- Seeded with the highest-value homelab containers, which also serve as the migration test corpus (doc 06): Jellyfin, Plex, the \*arr stack, qBittorrent, Immich, Nextcloud, Home Assistant, Vaultwarden, Paperless-ngx, Uptime Kuma, Gitea, Grafana/Prometheus, Pi-hole/AdGuard, Nginx Proxy Manager, Syncthing, Audiobookshelf
- Grown in order of what homelab users run most, every template written from the application's upstream documentation and image — never copied or adapted from another catalog's template
- **Preferred images:** the application's official image, or the [linuxserver.io](https://www.linuxserver.io/) image where one exists. linuxserver.io images are built consistently and documented image by image at `docs.linuxserver.io`, and their `PUID`/`PGID` convention is the one Q26 already uses. Each template links the image documentation it was written from.

**Every curated template must set sane pool-aware defaults**: appdata on cache, media on the pool, no unnecessary privileges, explicit tags rather than `:latest` where the upstream publishes versions, `PUID=99`/`PGID=100` where the image supports them.

### Template format (Q64)

A template is a directory — `templates/<id>/compose.yaml` plus its icon — and `compose.yaml` is **a valid Compose file with an `x-hoserva` extension block**. Compose ignores `x-` fields, so every template can be checked with `docker compose config` and run by hand; the block carries only what the install flow needs:

```yaml
services:
  jellyfin:
    image: lscr.io/linuxserver/jellyfin:<pinned tag>
    environment:
      PUID: "99"
      PGID: "100"
      TZ: ${TZ}
    volumes:
      - ${APPDATA}/jellyfin:/config
      - ${MEDIA}:/data/media
    ports:
      - ${WEBUI_PORT}:8096
    restart: unless-stopped

x-hoserva:
  schema: 1
  id: jellyfin
  revision: 4
  title: Jellyfin
  categories: [media]
  icon: icon.svg
  docs: https://docs.linuxserver.io/images/docker-jellyfin/
  webui: http://{host}:${WEBUI_PORT}
  inputs:
    APPDATA:    { kind: path, role: appdata, default: /mnt/cache/appdata }
    MEDIA:      { kind: path, role: share, label: Media library }
    WEBUI_PORT: { kind: port, default: 8096 }
    TZ:         { kind: timezone }
```

- **Inputs** are the only values the install form asks for. Each has a kind — `path`, `port`, `string`, `secret`, `timezone`, `device` — and a path also has a role (`appdata`, `share`, `media`, `downloads`) that drives share-aware path picking (doc 03 §5). A `device` input with role `gpu` offers the host's `/dev/dri` render devices; NVIDIA GPUs need the host driver and container toolkit as a prerequisite checked by `hoserva doctor`, and a GPU bound to a VM is never offered (Q82).
- **Secrets** (`kind: secret`) are generated at install time and written only to the stack's `.env`.
- **`revision`** increases with every change to a template. An installed stack records the source, id and revision it came from (§2), which is what "template update available" compares against.
- **The privilege summary is computed from the Compose content** (doc 01 §7) — privileged mode, host networking, the Docker socket, paths outside the pool — never declared by the template, so a template cannot understate what it asks for.
- **CI on every change to `templates/`:** the `x-hoserva` schema, `docker compose config`, image and tag existence, path conventions, and the privilege audit.

**Installing** resolves the inputs, writes the Compose file — keeping its `x-hoserva` block for later comparison — and `.env` into `/var/lib/hoserva/stacks/<name>/` (§2), and records the template's source, id and revision in `meta.json`.

### Distribution (Q65)

- **CI builds one signed catalog archive** from `templates/` on every merge: `catalog.tar.zst`, holding an `index.json` (each template's id, revision, metadata and content hash, plus the archive's serial) with the templates and icons, and a detached Ed25519 signature. It is published under `/catalog/` on the project site (Q66) — never served through the GitHub API. The URL is a setting with a compiled-in default on the project's own domain, so moving the catalog repository (Q39) changes nothing for installations.
- **Every installation has the catalog on disk.** `hoservad` embeds a snapshot at build time, so the first run and offline installs have a working catalog; the refreshed copy lives in `/var/lib/hoserva/catalog/`.
- **Refresh is one conditional request a day**, with random jitter, plus a manual refresh button. An unchanged catalog answers `304 Not Modified` and nothing is downloaded. GitHub's API rate limit never applies: nothing comes from `api.github.com`, and nothing is fetched per template. Refresh can be disabled; the on-disk copy keeps working.
- **Nothing unverified is used.** The archive replaces the on-disk copy only after its signature verifies against the public key compiled into `hoservad` and its serial is higher than the current one, so an older signed catalog cannot be replayed. A failed check keeps the previous catalog and raises a notification.
- **A catalog update never changes an installed app.** A newer revision shows as "template update available" with a diff against the installed Compose file; applying it is the user's action.
- **User-added sources** (§4) publish the same archive format. Their signature is optional, and an unsigned source is badged as unsigned.
