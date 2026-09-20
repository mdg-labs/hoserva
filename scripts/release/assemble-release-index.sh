#!/usr/bin/env bash
# Assembles https://hoserva.dev/releases/index.json (Q66, Q67) from
# release-index-entry.json fragments that publish-release.sh attaches to
# every GitHub Release. The URL is permanent: hoservad's update check
# reads only this file, never the GitHub API.
#
# Does not call the GitHub API. The caller (pages.yml, via
# fetch-release-index-entries.sh, or a test with fixtures) supplies a
# directory of already-fetched JSON files.
set -euo pipefail

usage() {
  echo "usage: $0 <entries-dir> <out-file>" >&2
  exit 1
}

[ $# -eq 2 ] || usage
entries_dir="$1"
out_file="$2"

if [ ! -d "$entries_dir" ]; then
  echo "assemble-release-index.sh: entries dir is not a directory: $entries_dir" >&2
  exit 1
fi

python3 - "$entries_dir" "$out_file" <<'PY'
import json
import re
import sys
from pathlib import Path

entries_dir = Path(sys.argv[1])
out_file = Path(sys.argv[2])

TAG_RE = re.compile(r"^v(\d+)\.(\d+)\.(\d+)(?:-beta\.(\d+))?$")


def fail(msg: str) -> None:
    print(f"assemble-release-index.sh: {msg}", file=sys.stderr)
    raise SystemExit(1)


def version_key(tag: str) -> tuple[int, int, int, int]:
    m = TAG_RE.fullmatch(tag)
    if m is None:
        fail(f"tag {tag!r} is not a recognised release tag (want vX.Y.Z or vX.Y.Z-beta.N)")
    major, minor, patch, beta = m.groups()
    return (int(major), int(minor), int(patch), int(beta) if beta is not None else 0)


def debian_version_from_tag(tag: str) -> str:
    m = TAG_RE.fullmatch(tag)
    if m is None:
        fail(f"tag {tag!r} is not a recognised release tag (want vX.Y.Z or vX.Y.Z-beta.N)")
    version = f"{m.group(1)}.{m.group(2)}.{m.group(3)}"
    if m.group(4) is not None:
        version += f"~beta.{m.group(4)}"
    return version


def require_asset(entry: dict, arch: str, path: Path) -> None:
    assets = entry.get("assets")
    if not isinstance(assets, dict) or arch not in assets:
        fail(f"{path}: missing assets.{arch}")
    asset = assets[arch]
    if not isinstance(asset, dict):
        fail(f"{path}: assets.{arch} must be an object")
    for field in ("url", "sha256"):
        if not asset.get(field):
            fail(f"{path}: missing assets.{arch}.{field}")


seen_tags: set[str] = set()
by_channel: dict[str, list[dict]] = {"stable": [], "beta": []}

for path in sorted(p for p in entries_dir.iterdir() if p.suffix == ".json" and p.is_file()):
    try:
        entry = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as e:
        fail(f"{path}: invalid JSON: {e}")
    if not isinstance(entry, dict):
        fail(f"{path}: entry must be a JSON object")

    for field in ("tag", "version", "channel"):
        if not entry.get(field):
            fail(f"{path}: missing {field}")

    tag = entry["tag"]
    channel = entry["channel"]
    if channel not in by_channel:
        fail(f"{path}: channel {channel!r} is not 'stable' or 'beta'")

    key = version_key(tag)
    expected = "beta" if key[3] else "stable"
    if channel != expected:
        fail(f"{path}: channel {channel!r} does not match tag {tag!r}")

    expected_version = debian_version_from_tag(tag)
    if entry["version"] != expected_version:
        fail(f"{path}: version {entry['version']!r} does not match tag {tag!r} (want {expected_version!r})")

    if tag in seen_tags:
        fail(f"duplicate tag {tag!r}")
    seen_tags.add(tag)

    require_asset(entry, "amd64", path)
    require_asset(entry, "arm64", path)
    by_channel[channel].append(entry)

for channel, entries in by_channel.items():
    entries.sort(key=lambda e: version_key(e["tag"]), reverse=True)

out_file.parent.mkdir(parents=True, exist_ok=True)
out_file.write_text(
    json.dumps({"channels": by_channel}, indent=2) + "\n",
    encoding="utf-8",
)
PY
