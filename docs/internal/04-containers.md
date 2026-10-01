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

Version policy (Q38): negotiate the Docker Engine API version at runtime rather than pinning an Engine release number; require the Compose v2 plugin; `hoserva doctor` warns when the installed Engine is older than the newer of the oldest release upstream still supports and the first release without a published container-escape advisory (currently 29.5.1); the warning never blocks Apps. Documentation points to Docker's own apt repository.

**Storage backend: a plain directory, never a loopback image (Q62).** Docker's data-root points at a directory on cache (`/mnt/cache/docker`), using the Engine's standard `overlay2` driver. Hoserva never creates a fixed-size loopback image for Docker's own storage — that construction is Unraid-specific, and a full loopback image needing a manual resize is one of its most common support complaints. A plain directory has no size of its own to run out of; it is sized by the cache device, which already has its own capacity monitoring and alerting (doc 02 §3). The move is offered, never applied silently: with no cache disk, or when Docker already holds containers, images, named volumes, user-defined networks or plugins, the data-root stays at `/var/lib/docker` (Q76). A named volume, user-defined network or installed plugin blocks the move even with no containers or images and no decision of its own to make — moving the data-root without it would strand its data, invisible to Docker, under the old root (#413, #416). Docker starts only once storage is up, through a managed systemd drop-in (Q69).

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

1. **Parse** with a `docker run` flag parser, not a regex, then **resolve every flag to its long form** before anything is translated:
   - A short flag becomes its `docker run` long form: `-v`→`--volume`, `-p`→`--publish`, `-e`→`--env`, `-u`→`--user`, `-h`→`--hostname`, `-m`→`--memory`, `-w`→`--workdir`, `-i`→`--interactive`, `-t`→`--tty`. A combined `-it` or `-ti` splits into `--interactive` and `--tty`.
   - `--flag=value` and `--flag value` are the same flag with the same value, and so are `-m=512m`, `-m 512m` and `-m512m`.
   - Any other short flag is unknown and handled by step 3.
2. **Translate known flags** to Compose equivalents with the table below, which is keyed by long form only.
3. **Never silently drop.** Unknown flags are emitted as a comment in the generated Compose file *and* surfaced as a warning in the install preview:
   ```yaml
   # Hoserva: could not translate the following Unraid ExtraParams:
   #   --some-exotic-flag=value
   # Review and add the Compose equivalent manually if required.
   ```
4. **Never interpolate into a shell.** These strings come from third parties; they are parsed into structured Compose fields, never concatenated into a command line.

**Translate table** — every flag below counts as translated for the clean-conversion metric (Q36); a flag whose value cannot be expressed in its Compose field is untranslated and goes through step 3 like an unknown flag.

| `docker run` flag | Compose field | Note |
|---|---|---|
| `--restart` | `restart` | |
| `--memory` | `mem_limit` | |
| `--memory-swap` | `memswap_limit` | |
| `--cpus` | `cpus` | |
| `--pids-limit` | `pids_limit` | |
| `--user` | `user` | |
| `--workdir` | `working_dir` | |
| `--hostname` | `hostname` | |
| `--group-add` | `group_add` | |
| `--entrypoint` | `entrypoint` | A static override shown in the side-by-side review like any other field. Docker takes the value as one executable, never split on spaces, so it becomes a one-element list (`/opt/my app/start` → `["/opt/my app/start"]`); an empty value, which clears the image's entrypoint, becomes `entrypoint: []`. Arguments stay in `<PostArgs>` → `command` |
| `--interactive` | `stdin_open` | |
| `--tty` | `tty` | |
| `--init` | `init` | |
| `--read-only` | `read_only` | |
| `--device` | `devices` | |
| `--cap-add` | `cap_add` | |
| `--cap-drop` | `cap_drop` | |
| `--security-opt` | `security_opt` | |
| `--sysctl` | `sysctls` | |
| `--ulimit` | `ulimits` | |
| `--dns` | `dns` | |
| `--add-host` | `extra_hosts` | |
| `--tmpfs` | `tmpfs` | |
| `--shm-size` | `shm_size` | |
| `--runtime` | `runtime` | |
| `--gpus` | `deploy.resources.reservations.devices` | |
| `--log-opt` | `logging.options` | |
| `--stop-timeout` | `stop_grace_period` | Seconds converted to a duration |
| `--health-cmd` | `healthcheck.test` | `["CMD-SHELL", <string>]` — see below |
| `--health-interval`, `--health-timeout`, `--health-retries`, `--health-start-period` | `healthcheck.interval`, `.timeout`, `.retries`, `.start_period` | |
| `--no-healthcheck` | `healthcheck.disable: true` | |
| `--volume`, `--mount` | `volumes` | `--mount` becomes Compose long syntax; merged as below |
| `--publish` | `ports` | Merged as below |
| `--env` | `environment` | Merged as below; `--env KEY` with no value inherits from the host environment, which a Compose file cannot express, so it is untranslated |
| `--pid` | `pid` | `host` and `container:<name>` are privilege-summary items |
| `--cgroupns` | `cgroup` | `host` is a privilege-summary item |
| `--device-cgroup-rule` | `device_cgroup_rules` | Always a privilege-summary item |

**`--health-cmd` is data.** The string is carried into `healthcheck.test` as written. It is a command for the container's own engine to run inside the container; Hoserva never runs it on the host.

**Merging with `<Config>` entries.** `--volume`, `--mount`, `--publish` and `--env` from `ExtraParams` add to the same `volumes`, `ports` and `environment` the `<Config>` entries produce. Their volume sources go through the same Path handling flags as a `<Config Type="Path">`: a source outside the pool and cache is flagged for manual review, not silently translated. Two entries with the same target are handled one way, whichever source they came from:

- **Exact duplicate** — the whole normalized entry is identical: for a volume the host path, container path, access mode (`ro`/`rw`) and every other mount option; for a port the host bind address, host port, container port and protocol; for a variable the name and value. The `ExtraParams` copy is dropped and an informational note is shown.
- **Same target, different value** — the same container path, container port and protocol, or variable name, with anything else in the entry different (a volume from a different host path or mounted `ro` instead of `rw`, a port on a different host port or bound to `127.0.0.1` instead of every address, a variable with a different value). Access mode and bind address change what the container may write and who can reach it, so they are never treated as a match. The `<Config>` entry is kept, the `ExtraParams` one is left out of the generated file, and the conflict is a review warning listing both. Conflicting values are never silently resolved, and the warning means the conversion is not clean.

**Privilege-widening flags.** `--pid=host`, `--pid=container:…`, `--cgroupns=host` and every `--device-cgroup-rule` translate, and each is a privilege-summary item in the same way as `<Privileged>`: the summary is computed from the generated Compose content (Q64), so `pid`, `cgroup` and `device_cgroup_rules` are named in it beside privileged mode, host networking and the Docker socket (§7, doc 03 §5.3, doc 01 §7).

Flags outside the table — `--label`, `--env-file`, `--privileged`, `--cpu-shares`, `--stop-signal` and the rest — are unknown flags under step 3 until a later change to this table adds them.

### Other known-hard cases

- **macvlan / custom networks** — require a pre-existing equivalent network. The converter detects the reference and flags it with the exact `docker network create` command, rather than guessing or creating networks silently; v1 has no network-creation UI (Q37).
- **Unraid-specific variables** like `$$` substitutions and `HOST_OS` are recognised and handled or flagged.
- **Multi-container templates** (the "AIO" pattern, bundling app + database + worker) map naturally to multi-service Compose, which expresses them directly.
- **`:latest` tags** — carried through as-is, but flagged in the UI as a reproducibility risk, since a portion of the ecosystem pins nothing.
- **Writable-layer state** — anything written inside the running container after launch (`docker exec`-ed in, hand-patched, or written outside a mapped volume) lives only in the container's writable layer, which no XML field expresses: the template describes how the container is *launched*, never what happened inside it afterwards. The converter cannot see this state and says so plainly, every time, rather than staying silent: it warns that the source container may hold configuration the generated Compose file does not reproduce, and that the user should check for it — inside mapped volumes, `docker exec`-ed patches, or manual file edits — before recreating the container. **v1 ships the warning only; automated detection is deferred.** Detection, if ever built, would compare the writable layer against the image (the `docker diff` relationship) and report paths changed outside mapped volumes, excluding known noise (`/tmp`, `/var/log`, `/var/cache`, `/run`, package-manager state) — but that needs the source `docker.img` mounted and a noise-exclusion list that will need tuning, so it is out of scope here. A documented warning is cheap, honest, and creates no false confidence. This does not change what counts as a "clean" conversion (Q36): Q36 measures how completely the XML translates to Compose, not what state the source container held, and folding this warning into that metric would distort the release number it gates.

### Output is always reviewable

Generated Compose is shown side by side with the source XML before anything runs, with all warnings listed. **Writable-layer state is its own warning class** in that review, separate from translation warnings (untranslated `ExtraParams`, unresolved networks, paths flagged for manual review) — never a silent pass. Never a silent conversion followed by a container that behaves subtly differently.

---

## 6. Update handling

- Check registries at most once a day, with jitter, by requesting each image's manifest and, for an image whose tag looks like a version (`16.4`, `v1.2.3-alpine`, never `latest`), the repository's tag list — never pulling an image or fetching a blob; optional per-registry credentials stored as secrets; a registry that rate-limits either request is skipped until the next day, and the UI says so (Q81)
- Distinguish **digest changed on the same tag** (the common `:latest` case) from **a genuinely new version tag**
- Show both, labelled differently — "new build of `latest`" is not the same as "2.1 → 2.2"
- Bulk update with per-container opt-out
- **Pre-update appdata snapshot** for containers whose appdata sits on the cache, so a bad update is recoverable. A bad container update is one of the most common ways a working homelab service breaks. The image is pulled first, which changes nothing about the container, so a failed pull or a pull that finds nothing newer costs no snapshot and no downtime; the snapshot (doc 10 §2) is taken after it and before the container is replaced.
- Rollback: keep the previous image locally for a configurable period and offer a one-click revert. The period is the `imageKeepDays` app setting, 1 to 365 days and 7 by default (Q88). A revert stops the container if it runs, restores the snapshot, recreates the container from the kept image without pulling, and starts it once on that image, so the updated image never runs against the restored data. A revert that fails after it stopped the container leaves it stopped, and reverting again finishes it for as long as the snapshot is still on its destination (the restore never prunes the snapshot it reads, but a later archive can push it out of the 5 newest), after which the container is started by hand.

---

## 7. Curated catalog

Hoserva's catalog is its own template repository (D19) — `mdg-labs/hoserva-catalog`, separate from this monorepo from the start (doc 12 §7, Q39). It is the only built-in catalog source. It is licensed MIT and takes contributions under the same DCO sign-off as this repository (Q35), and template requests and template PRs are tracked there, while the schema, validator and fetch-and-verify code stay in this repository.

### What goes in it

- Seeded with the highest-value homelab containers: Jellyfin, Plex, the \*arr stack, qBittorrent, Immich, Nextcloud, Home Assistant, Vaultwarden, Paperless-ngx, Uptime Kuma, Gitea, Grafana/Prometheus, Pi-hole/AdGuard, Nginx Proxy Manager, Syncthing, Audiobookshelf
- Grown in order of what homelab users run most, every template written from the application's upstream documentation and image — never copied or adapted from another catalog's template
- **Preferred images:** the application's official image, or the [linuxserver.io](https://www.linuxserver.io/) image where one exists. linuxserver.io images are built consistently and documented image by image at `docs.linuxserver.io`, and their `PUID`/`PGID` convention is the one Q26 already uses. Each template links the image documentation it was written from.
- **The linuxserver.io image API** (`https://api.linuxserver.io/api/v1/images`, unauthenticated JSON) publishes the same per-image data in structured form: name, description, category, project and GitHub URLs, version, `stable` and `deprecated` flags, `stars`, `monthly_pulls`, tags and architectures, and with `?include_config=true` a `config` object listing each image's environment variables, volumes and ports (with `/udp` ports) and an `application_setup` link. It plays two roles:
  - **Authoring input.** For a template that uses a linuxserver.io image, the API is that image's own documentation in structured form, and an author may take the image's ports, volumes and environment variables from it. D19 still holds: the template is written from the image's documentation, still links its `docs.linuxserver.io` page, and applies Hoserva's defaults on top (appdata on cache, media on the pool, `PUID=99`/`PGID=100` per Q26, pinned tags). The API's example host paths such as `/path/to/...` and its `1000` IDs are placeholders, not defaults.
  - **Prioritisation input,** alongside `selfhst/cdn` below: `monthly_pulls` and `stars` show demand, and `deprecated` and `stable` rule out images that should not get a new template.
  - **Limits.** It is never fetched at runtime by `hoservad` and never shipped as a suggestion feed or catalog source (D19). It has no published data licence, so it guides authorship and its data is never redistributed: nothing copies its descriptions or `project_logo` images into the catalog. It covers linuxserver.io images only; a template built on an application's official image is written from that application's own documentation. linuxserver.io's separate `docker-templates` repository (Unraid XML templates) is another catalog's templates and is never a source.
- **Choosing and ordering apps: `selfhst/cdn`** (MIT, © 2025 selfh.st) publishes the selfh.st app directory as data: `directory/software.json` lists about 1,340 self-hosted apps with repo URL, license, category tags, GitHub stars, last-update date and a maintenance status, and `directory/companions.json` about 190 helper tools linked to their parent apps. It is a **planning input** for which apps get a template and in what order — the stars and maintenance status behind "grown in order of what homelab users run most". It is never a template source: it carries no image, port, volume or environment data, and D19 requires every template to be written from the application's upstream documentation. It is never fetched at runtime or shipped as a suggestion feed, because that would make it a built-in third-party catalog source, which D19 rules out. A script that reads it checks the column count and fails loudly if it changes, because its rows are positional arrays with no header or documented format; entries whose license is `Custom` or `Other` are checked by hand. Its companion `selfhst/icons` repository is CC-BY-4.0, not MIT, and the logos remain their projects' trademarks, so icon sourcing is decided separately.

**Every curated template must set sane pool-aware defaults**: appdata on cache, media on the pool, no unnecessary privileges, explicit tags rather than `:latest` where the upstream publishes versions, `PUID=99`/`PGID=100` where the image supports them.

### Template format (Q64)

A template is a directory in the catalog repository's `templates/` folder — `templates/<id>/compose.yaml` plus its icon — and the published archive keeps `<id>/` at its root, next to `index.json`. `compose.yaml` is **a valid Compose file with an `x-hoserva` extension block**. Compose ignores `x-` fields, so every template can be checked with `docker compose config` and run by hand; the block carries only what the install flow needs:

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
- **Optional inputs.** A `string` input may declare `optional: true`: it may be left empty, and an empty value is written to `.env` as `NAME=` instead of being refused. An optional input has no `default`, because an empty value would then be replaced by it, so it could never be left empty. Every other kind needs a value — a path, a port and a time zone always resolve to one, and a secret is generated — so `hoserva template lint` refuses `optional` on them. An empty optional input interpolated into a privilege-relevant key is resolved like any other empty value (a boolean key such as `privileged` refuses it).
- **Secrets** (`kind: secret`) are generated at install time and written only to the stack's `.env`.
- **`revision`** increases with every change to a template. An installed stack records the source, id and revision it came from (§2), which is what "template update available" compares against.
- **The privilege summary is computed from the Compose content** (doc 01 §7) — privileged mode, host networking, the host PID namespace, the host cgroup namespace, device cgroup rules, the Docker socket, paths outside the pool — never declared by the template, so a template cannot understate what it asks for.
- **The schema is a versioned contract between two repositories.** The `x-hoserva` schema and its validator live in this repository (`internal/template/`), and Hoserva also publishes the schema as a versioned JSON Schema that third-party catalog authors can use too. The `schema:` number is the compatibility boundary, and a newer Hoserva keeps reading older schema versions.
- **CI on every change to the catalog repository** runs Hoserva's own checker from a pinned Hoserva version — for example `hoserva template lint`, run through `go run …/cmd/hoserva@<pinned>` — and does not copy the rules. The checker covers the `x-hoserva` schema, path conventions and the privilege audit; the catalog CI adds `docker compose config` and image and tag existence, which need outside services and are the reason this CI does not gate a Hoserva release (doc 12 §7).

**Installing** resolves the inputs, writes the Compose file — keeping its `x-hoserva` block for later comparison — and `.env` into `/var/lib/hoserva/stacks/<name>/` (§2), and records the template's source, id and revision in `meta.json`.

### Distribution (Q65)

- **The catalog repository's CI builds one signed catalog archive** on every merge: `catalog.tar.zst`, holding an `index.json` (each template's id, revision, metadata and content hash, plus the archive's serial) with the templates and icons, and a detached Ed25519 signature. It is published from that repository's own GitHub Pages site at `catalog.hoserva.dev` (Q66) — never served through the GitHub API. The URL is a setting with a compiled-in default there, and the archive is trusted through its signature, not its host, so moving the catalog later changes nothing for installations.
- **The catalog has its own signing key**, separate from the release-signing key. The private half is the `HOSERVA_CATALOG_SIGNING_KEY` secret on the catalog repository and is held only by its CI; the public half is `signing-key.pub.pem` at the repository root. Before signing, CI checks that the secret's public half matches the published one, the same integrity check `release.yml` runs for the release key. `hoservad` compiles in two public keys, one for updates and one for the catalog, and checks each file only against its own (doc 01 §7).
- **Every installation has the catalog on disk.** `hoservad` embeds a snapshot at build time, so the first run and offline installs have a working catalog; the refreshed copy lives in `/var/lib/hoserva/catalog/`. The snapshot is a pinned archive: this repository commits one published archive's serial and SHA-256, and the build fetches that archive, checks the pin's SHA-256 and the signature, and embeds it. The build gets it from the catalog repository's GitHub Release `serial-<serial>` (assets `catalog.tar.zst` and `catalog.tar.zst.sig`), because each Pages deploy replaces the whole site and `catalog.hoserva.dev` only ever serves the latest archive — a pin needs an immutable home, which is also what lets an old tag be rebuilt. Installations never use that release: they refresh from `catalog.hoserva.dev`, which is always the latest. Updating the snapshot is a normal commit that bumps the pin, so builds are reproducible and never embed an unverified archive. Tests that use curated templates, such as install resolution and the privilege summary, read this pinned snapshot, never a live fetch (doc 06 §2).
- **Refresh is one conditional request per check**, and every path is the same request with the same signature and serial checks. An unchanged catalog answers `304 Not Modified` and nothing is downloaded. GitHub's API rate limit never applies: the refresh fetches the latest archive from `catalog.hoserva.dev`, nothing comes from `api.github.com`, and nothing is fetched per template. The release download above is build-time only, never part of a refresh. The triggers:
  - **A background interval the user sets:** off, hourly, every 6 hours, every 12 hours or daily, with random jitter. Daily is the default.
  - **Check on open, user-set and on by default:** reading the catalog through the API starts a background conditional request when the last check is older than 15 minutes. The response is served from the on-disk copy immediately and never waits on the network, and completion is announced on `/api/v1/events` so the UI refreshes. The trigger lives in `hoservad`, so the CLI and the UI behave the same (D5).
  - **A manual check, always available:** *Check for updates* on `/apps/catalog` (doc 03 §5.2) runs one conditional request now and reports the outcome — new templates, updated templates, unchanged, or failed with the reason. It is an explicit user action, so it works even when both automatic triggers are off.
  - **Off means no automatic network traffic:** with the interval off and check-on-open off, Hoserva contacts the catalog host only when the user presses the button. The on-disk copy keeps working.

  The interval and the check-on-open switch are settings, stored as additive columns on the `schema_info` settings row next to `update_check_enabled`, not as a `schedule_jobs` row (doc 03 §8.4).
- **Nothing unverified is used.** The archive replaces the on-disk copy only after its signature verifies against the catalog public key compiled into `hoservad` and its serial is higher than the current one, so an older signed catalog cannot be replayed. A failed check keeps the previous catalog and raises a notification.
- **A catalog update never changes an installed app.** A newer revision shows as "template update available" with a diff against the installed Compose file; applying it is the user's action.
- **User-added sources** (§4) publish the same archive format. Their signature is optional, and an unsigned source is badged as unsigned.
