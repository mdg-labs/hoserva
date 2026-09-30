#!/usr/bin/env bash
# Create or update the issue label set on the GitHub repo.
#
# The label set is documented in CLAUDE.md ("Label set") and is deliberately
# small — github-triage and orchestrate both assume exactly these names, and
# scripts/issue-status.sh assumes the status:* family exists. Idempotent:
# `gh label create --force` updates colour and description on an existing
# label instead of failing, so re-running after editing this file is the way
# to change a label.
#
# Two sets: this repository's (the default) and the template catalog
# repository's, a small one for template requests. Select one with the
# argument `hoserva` or `catalog`, or by GH_REPO=mdg-labs/hoserva-catalog.
# `--dry-run` prints the selected set's names without calling gh.
set -euo pipefail

die() { printf '%s\n' "$*" >&2; exit 1; }

SET=""
DRY_RUN=0
for arg in "$@"; do
  case $arg in
    hoserva | catalog) SET=$arg ;;
    --dry-run) DRY_RUN=1 ;;
    *) die "usage: $0 [hoserva|catalog] [--dry-run]" ;;
  esac
done

if [[ -z $SET ]]; then
  case ${GH_REPO:-} in
    mdg-labs/hoserva-catalog) SET=catalog ;;
    *) SET=hoserva ;;
  esac
fi

case $SET in
  catalog) REPO=${GH_REPO:-mdg-labs/hoserva-catalog} ;;
  *) REPO=${GH_REPO:-mdg-labs/hoserva} ;;
esac

[[ $SET == hoserva && $REPO == mdg-labs/hoserva-catalog ]] && die "refusing the hoserva label set on $REPO"
[[ $SET == catalog && $REPO == mdg-labs/hoserva ]] && die "refusing the catalog label set on $REPO"

catalog_labels=(
  # name|colour|description
  "new-app|0e8a16|Request a template for an app the catalog does not have"
  "template-update|1d76db|Change to an existing template: new version, settings, fix"
  "broken-template|d73a4a|A catalog template does not deploy or run as described"
  "needs-upstream-docs|fbca04|Waiting on the app's upstream documentation, which templates are written from"
)

hoserva_labels=(
  # name|colour|description
  "feat|0e8a16|New capability"
  "bug|d73a4a|Something isn't working"
  "chore|fef2c0|Maintenance, tooling, CI"
  "docs|0075ca|Documentation only (docs/internal/ or site/)"
  "spike|c2e0c6|Time-boxed investigation; deliverable is recorded findings"

  "area:storage|1d76db|Disks, pool mounts, parity, threshold guard, mover (internal/disk,pool,parity,cache)"
  "area:api|006b75|API, store, jobs, config generation, notifications, daemon"
  "area:web|5319e7|Web UI (web/)"
  "area:cli|0e8a16|The hoserva CLI (cmd/hoserva)"
  "area:shares|bfdadc|SMB/NFS shares, users, permissions (internal/share)"
  "area:containers|0052cc|Container management, templates, catalog, converter"
  "area:vm|5b0bb5|VM management: libvirt/KVM, passthrough (internal/vm) — not the L3 test-VM harness"
  "area:migration|b60205|Unraid migration (internal/migrate)"
  "area:backup|fbca04|Config and appdata backup (internal/backup)"
  "area:packaging|e99695|.deb, apt repo, ISO (packaging/, scripts/release)"
  "area:devenv|c5def5|Loop-device lab, VMs, test corpora, CI workflows"
  "area:site|d4c5f9|Public docs site (site/)"

  "epic|3e4b9e|Milestone issue with sub-issues"
  "safety-critical|b60205|Touches a path where a bug loses data — human reads the diff before push"
  "needs-sudo|d93f0b|Requires root — maintainer runs this, never an agent"
  "blocked|000000|Cannot proceed until a dependency or external blocker is resolved"

  "status:new|ededed|Filed, not yet triaged"
  "status:ready|bfd4f2|Triaged and ready to be picked up"
  "status:in-progress|fbca04|An executor is implementing this"
  "status:in-review|d4c5f9|Implementation committed, awaiting verification"
  "status:implemented|c2e0c6|Verification passed, awaiting the landing commit"
  "status:closed|586069|Closed as completed"
  "status:cancelled|8b4513|Closed as not planned, or as a duplicate"
)

if [[ $SET == catalog ]]; then
  labels=("${catalog_labels[@]}")
else
  labels=("${hoserva_labels[@]}")
fi

for entry in "${labels[@]}"; do
  IFS='|' read -r name colour description <<<"$entry"
  if ((DRY_RUN)); then
    printf '%s\n' "$name"
    continue
  fi
  gh label create "$name" --repo "$REPO" --color "$colour" --description "$description" --force >/dev/null
  printf '%s\n' "$name"
done
