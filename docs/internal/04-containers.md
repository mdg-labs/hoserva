# Hoserva — Container Management and Templates

---

## 1. Decision

**Build container management, but deliberately narrow. Not a Portainer clone.**

### Why build it

The app catalog is Unraid's actual killer feature. Community Applications is why many users stay on a paid product they otherwise complain about. Anyone migrating expects: pick a template → review paths and ports → running.

Deferring this ("just install Portainer") breaks the migration at exactly the point where it hurts, and reduces Hoserva to a storage manager competing with OMV rather than with Unraid.

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
/var/lib/hoserva/stacks/<name>/meta.json     # Hoserva metadata: template source, icon, install time
```

Driven through the Docker Engine API and `docker compose`. No proprietary format, no exclusive claim on the daemon.

Consequences, all intentional:

- Portainer, Dockge, or Dokploy can run alongside and manage the same containers
- Containers created outside Hoserva appear as **unmanaged** — visible, lifecycle actions available, untouched by the template system
- A user can `cd` into a stack directory and run `docker compose` by hand
- Uninstalling Hoserva leaves every container running and every Compose file intact — including `dpkg --purge`: the package's purge script removes Hoserva's own state but never `/var/lib/hoserva/stacks/`

**The user is never locked in.** This should be stated in the marketing copy, because it is the direct counter to the main objection to Unraid's Docker implementation.

---

## 3. Prerequisite handling

Docker Engine and the Compose plugin must be present. The `.deb` does not install them (decision D8) — Docker's own repository is the correct source, and pulling it in as a dependency creates version conflicts on systems that already have it.

Behaviour:

- `postinst` checks and prints install instructions if missing
- `hoservad` starts regardless, but the Apps section shows a clear prerequisite banner with the exact commands
- `hoserva doctor` reports version and API reachability
- The ISO bundle (doc 07 §1, Phase 4) ships both preinstalled

Version policy (Q38): negotiate the Docker Engine API version at runtime rather than pinning an Engine release number; require the Compose v2 plugin; `hoserva doctor` warns when the installed Engine is a release upstream no longer supports. Documentation points to Docker's own apt repository.

---

## 4. The Unraid Community Applications feed — findings

**Checked, and the answer is yes: there is a public, machine-readable feed.**

The Community Applications ecosystem is built on a set of public JSON artifacts in the `Squidly271/AppFeed` repository, which contains `applicationFeed.json`, `applicationFeed-raw.json`, `applicationFeed-lastUpdated.json`, `blacklistedRepos.json`, `categoryList.json`, `containerStats.json`, and `duplicatedTemplates.json`, plus a `repositories` directory — with over 21,000 commits, reflecting a continuously regenerated feed.

Relevant details:

- **The aggregated feed is the whole catalog.** `applicationFeed.json` is the processed feed CA itself consumes; `applicationFeed-raw.json` is the pre-processing form. Entries reference the source template path, e.g. `templates/<repositoryName>/<App>/<App>.xml`.
- **It is CDN-mirrored.** CA's own code references `https://cdn.jsdelivr.net/gh/Squidly271/AppFeed@master/applicationFeed-lastUpdated.json`, so the `applicationFeed.json` sibling is reachable the same way — a polite, cacheable fetch path that doesn't hammer GitHub.
- **A last-updated endpoint exists**, so Hoserva can poll cheaply and only pull the full feed when it changes.
- **There is a separate moderation layer.** `Squidly271/Community-Applications-Moderators` holds `Repositories.json`, the list of contributing template repositories with maintainer names and contact method, alongside `Moderation.json`, which carries moderator comments, deprecation markers (`DeprecatedMaxVer`), blacklist entries, `RemoveFromCA`, and version-incompatibility flags.

  **This matters more than the feed itself.** It is the mechanism by which malicious, abandoned, and broken templates get flagged. A catalog that consumed `applicationFeed.json` while ignoring `Moderation.json` and `blacklistedRepos.json` would happily offer users templates the Unraid community has already removed for cause. **Consuming the moderation data is mandatory, not optional.**
- **An older LinuxServer-hosted feed** at `tools.linuxserver.io/unraid-docker-templates.json` also exists, but it predates the current CA infrastructure and should be treated as legacy.
- **Prior art for the conversion itself exists**: the `unraid-templates` GitHub topic lists a project for automatically converting community application docker templates to docker compose, and selfhosters maintains `docker-compose-to-UR-template`, a Python script that reads a docker-compose file and generates an Unraid template — the same mapping in the opposite direction. Both are worth reading before writing the converter.

### Licensing — must be resolved before shipping

Individual template repositories carry their own licenses. IBRACORP's, for example, is GPL v3, permitting personal and commercial use and modification and distribution under the same terms, requiring source disclosure for modifications, while others are MIT, with each packaged application keeping its own upstream license.

So the templates are not uniformly licensed, and the aggregated feed's own license status needs checking separately from the templates it indexes.

**Recommended posture** (tracked as Q33–Q35 in doc 13):

1. **Do not redistribute.** Hoserva fetches the feed at runtime from the upstream CDN rather than vendoring a copy. This sidesteps most redistribution questions.
2. **Attribute visibly.** Every catalog entry shows its source repository and maintainer, linking upstream. Templates imported from CA are badged as such, not presented as Hoserva content.
3. **Honour the moderation data**, including removals and blacklists — both an ethical and a safety requirement.
4. **Make it opt-in.** The CA feed is a toggleable catalog source, off by default on first run, with a one-screen explanation of where the templates come from and that they are community-maintained and unvetted by Hoserva.
5. **Contact the maintainer.** Squid (Squidly271) maintains this infrastructure personally. A message before building on it is both courteous and likely to surface constraints that aren't documented. This costs one email and could prevent the feature being pulled after launch.
6. **Get the aggregated feed's license reviewed** properly before it becomes load-bearing.

### Fallback if this path closes

If CA consumption turns out to be unacceptable, the fallback is a Hoserva-native catalog: its own Git repository of templates, community PRs, seeded from the most-installed containers. Slower to reach parity, fully under control. **The converter remains valuable either way**, because migrating users still need their own `templates-user/` directory converted — that is local user data, with no licensing question at all.

**Design implication:** build the catalog with a pluggable source interface from day one — Hoserva-native repo, CA feed, and user-added repository URLs all as sources behind one abstraction. Then the licensing outcome changes a config default rather than an architecture.

---

## 5. Unraid XML template converter

### Source

Per-container XML files under `/boot/config/plugins/dockerMan/templates-user/` on an Unraid flash drive, and the same format in the CA feed.

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
- **Multi-container templates** (the "AIO" pattern, bundling app + database + worker) map naturally to multi-service Compose, which is actually easier in Compose than in Unraid's one-container-per-template model.
- **`:latest` tags** — carried through as-is, but flagged in the UI as a reproducibility risk, since a portion of the ecosystem pins nothing.

### Output is always reviewable

Generated Compose is shown side by side with the source XML before anything runs, with all warnings listed. Never a silent conversion followed by a container that behaves subtly differently.

---

## 6. Update handling

- Poll registries for new tags on the configured schedule, respecting rate limits
- Distinguish **digest changed on the same tag** (the common `:latest` case) from **a genuinely new version tag**
- Show both, labelled differently — "new build of `latest`" is not the same as "2.1 → 2.2"
- Bulk update with per-container opt-out
- **Pre-update appdata snapshot** for containers whose appdata sits on the cache, so a bad update is recoverable. This is the single most-requested thing missing from Unraid's update flow.
- Rollback: keep the previous image locally for a configurable period and offer a one-click revert

---

## 7. Curated catalog

Independent of the CA feed question, Hoserva maintains its own template repository — `templates/` in the monorepo until the first external template PR, then split out (doc 12 §7, Q39). It is the default catalog source on a fresh install (Q33):

- Templates as YAML (more readable than XML, diffable in PRs)
- Community contributions via pull request with CI validation: schema check, image existence check, path convention check, privilege audit
- Seeded with the highest-value homelab containers, which also serve as the migration test corpus (doc 06): Jellyfin, Plex, the \*arr stack, qBittorrent, Immich, Nextcloud, Home Assistant, Vaultwarden, Paperless-ngx, Uptime Kuma, Gitea, Grafana/Prometheus, Pi-hole/AdGuard, Nginx Proxy Manager, Syncthing, Audiobookshelf

**Every curated template must set sane pool-aware defaults**: appdata on cache, media on the pool, no unnecessary privileges, explicit tags rather than `:latest` where the upstream publishes versions, `PUID=99`/`PGID=100` where the image supports them.

---

## Sources

The research behind §4 originally carried citation markers without URLs; those markers have been removed. Primary sources to re-link before any of it is quoted publicly:

- Community Applications feed: `github.com/Squidly271/AppFeed`
- CA moderation data: `github.com/Squidly271/Community-Applications-Moderators`
- Legacy feed: `tools.linuxserver.io/unraid-docker-templates.json`
- Individual template repositories' own `LICENSE` files (licensing varies per repository)
