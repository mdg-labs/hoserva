#!/usr/bin/env bash
# Unit tests for the GitHub Pages assembly path (Q66): placeholder
# root, /catalog/ and /releases/ as distinct trees, the 900 MiB
# fail-before-deploy canary, no /apt/, and fetch of
# release-index-entry.json assets through a fake `gh` — never the live
# GitHub API.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

fail=0
note() { printf 'test-pages-site: %s\n' "$*" >&2; }
assert_eq() {
  if [ "$1" != "$2" ]; then
    note "FAIL: expected '$2', got '$1' ($3)"
    fail=1
  fi
}
assert_file() {
  if [ ! -f "$1" ]; then
    note "FAIL: expected file $1 ($2)"
    fail=1
  fi
}
assert_no_path() {
  if [ -e "$1" ]; then
    note "FAIL: expected $1 to be absent ($2)"
    fail=1
  fi
}
assert_contains() {
  if ! grep -Fq "$2" "$1"; then
    note "FAIL: $1 does not contain '$2' ($3)"
    fail=1
  fi
}

for s in assemble-pages-site.sh assemble-release-index.sh fetch-release-index-entries.sh; do
  bash -n "$script_dir/$s" || {
    note "FAIL: $s is not valid shell (bash -n)"
    fail=1
  }
done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

write_entry() {
  local dest="$1" tag="$2" version="$3" channel="$4"
  cat >"$dest" <<JSON
{
  "tag": "${tag}",
  "version": "${version}",
  "channel": "${channel}",
  "assets": {
    "amd64": {"url": "https://github.com/mdg-labs/hoserva/releases/download/${tag}/hoserva_${version}_amd64.deb", "sha256": "aa"},
    "arm64": {"url": "https://github.com/mdg-labs/hoserva/releases/download/${tag}/hoserva_${version}_arm64.deb", "sha256": "bb"}
  }
}
JSON
}

# Empty index, placeholder root, catalog stub, CNAME, .nojekyll, no apt.
empty_entries="$work/empty-entries"
mkdir -p "$empty_entries"
out0="$work/out0"
mkdir -p "$out0"
"$script_dir/assemble-pages-site.sh" "$out0" "$empty_entries"
assert_file "$out0/index.html" "placeholder"
assert_contains "$out0/index.html" "https://github.com/mdg-labs/hoserva" "placeholder points at the repository"
assert_contains "$out0/index.html" "https://hoserva.dev/releases/index.json" "permanent release-index URL"
assert_contains "$out0/index.html" "https://hoserva.dev/catalog/" "permanent catalog URL"
assert_file "$out0/catalog/index.html" "catalog stub"
assert_file "$out0/releases/index.json" "empty index"
assert_file "$out0/CNAME" "custom domain"
assert_eq "$(cat "$out0/CNAME")" "hoserva.dev" "CNAME"
assert_file "$out0/.nojekyll" "disable Jekyll"
assert_no_path "$out0/apt" "no apt tree on a placeholder site"
assert_eq "$(python3 -c 'import json,sys; print(json.load(sys.stdin)["channels"]["stable"])' <"$out0/releases/index.json")" "[]" "empty stable channel"
assert_eq "$(python3 -c 'import json,sys; print(json.load(sys.stdin)["channels"]["beta"])' <"$out0/releases/index.json")" "[]" "empty beta channel"

# Version sort (v0.10.0 after v0.9.0), both channels, catalog contents
# preserved, docs root does not clobber /catalog/ or /releases/.
entries="$work/entries"
mkdir -p "$entries"
write_entry "$entries/a.json" "v0.9.0" "0.9.0" "stable"
write_entry "$entries/b.json" "v0.10.0" "0.10.0" "stable"
write_entry "$entries/c.json" "v0.10.0-beta.2" "0.10.0~beta.2" "beta"
write_entry "$entries/d.json" "v0.10.0-beta.12" "0.10.0~beta.12" "beta"

docs="$work/docs"
mkdir -p "$docs/getting-started" "$docs/catalog" "$docs/releases" "$docs/apt/pool"
echo 'DOCS-ROOT' >"$docs/index.html"
echo 'getting-started' >"$docs/getting-started/index.html"
echo 'from-docs-catalog' >"$docs/catalog/evil.html"
echo 'from-docs-releases' >"$docs/releases/index.json"
echo 'pool' >"$docs/apt/pool/x.deb"

catalog="$work/catalog"
mkdir -p "$catalog"
echo 'real-catalog' >"$catalog/catalog.tar.zst"

out1="$work/out1"
mkdir -p "$out1"
"$script_dir/assemble-pages-site.sh" "$out1" "$entries" "$catalog" "$docs"
assert_contains "$out1/index.html" "DOCS-ROOT" "docs root kept"
assert_contains "$out1/getting-started/index.html" "getting-started" "docs subtree kept"
assert_file "$out1/catalog/catalog.tar.zst" "catalog tree from catalog-dir"
assert_contains "$out1/catalog/catalog.tar.zst" "real-catalog" "catalog payload"
assert_no_path "$out1/catalog/evil.html" "docs must not land in /catalog/"
assert_no_path "$out1/releases/from-docs" "docs must not land in /releases/"
assert_no_path "$out1/apt" "apt stripped from the Pages artifact"
assert_eq "$(python3 -c 'import json,sys; c=json.load(sys.stdin)["channels"]; print(c["stable"][0]["tag"], c["stable"][1]["tag"], c["beta"][0]["tag"], c["beta"][1]["tag"])' <"$out1/releases/index.json")" \
  "v0.10.0 v0.9.0 v0.10.0-beta.12 v0.10.0-beta.2" \
  "channels grouped and version-sorted newest first"

# 900 MiB canary: a tiny limit must fail before anything would deploy.
out2="$work/out2"
mkdir -p "$out2"
if HOSERVA_PAGES_MAX_BYTES=1 "$script_dir/assemble-pages-site.sh" "$out2" "$empty_entries" >/dev/null 2>"$work/size.err"; then
  note "FAIL: assemble-pages-site.sh should refuse an artifact over HOSERVA_PAGES_MAX_BYTES"
  fail=1
else
  if ! grep -q 'refusing to deploy' "$work/size.err"; then
    note "FAIL: size-canary error should say it is refusing to deploy"
    fail=1
  fi
fi

# Invalid JSON, missing field, duplicate tag, channel/tag mismatch.
bad="$work/bad"
mkdir -p "$bad"
echo '{not json' >"$bad/x.json"
if "$script_dir/assemble-release-index.sh" "$bad" "$work/bad-out.json" >/dev/null 2>&1; then
  note "FAIL: invalid JSON should be refused"
  fail=1
fi
rm -f "$bad/x.json"
write_entry "$bad/x.json" "v0.1.0" "0.1.0" "stable"
python3 -c 'import json,pathlib; p=pathlib.Path("'"$bad"'/x.json"); d=json.loads(p.read_text()); del d["assets"]["arm64"]; p.write_text(json.dumps(d))'
if "$script_dir/assemble-release-index.sh" "$bad" "$work/bad-out.json" >/dev/null 2>&1; then
  note "FAIL: missing assets.arm64 should be refused"
  fail=1
fi
rm -f "$bad/x.json"
write_entry "$bad/one.json" "v0.1.0" "0.1.0" "stable"
write_entry "$bad/two.json" "v0.1.0" "0.1.0" "stable"
if "$script_dir/assemble-release-index.sh" "$bad" "$work/bad-out.json" >/dev/null 2>&1; then
  note "FAIL: duplicate tag should be refused"
  fail=1
fi
rm -f "$bad/one.json" "$bad/two.json"
write_entry "$bad/mismatch.json" "v0.1.0" "0.1.0" "beta"
if "$script_dir/assemble-release-index.sh" "$bad" "$work/bad-out.json" >/dev/null 2>&1; then
  note "FAIL: channel/tag mismatch should be refused"
  fail=1
fi

# fetch-release-index-entries.sh against a fake gh: one recognised tag
# with the asset, one recognised tag without it, one unrecognised tag.
mock_bin="$work/mock-bin"
mkdir -p "$mock_bin" "$work/gh-assets/v0.1.0" "$work/gh-assets/v0.2.0-beta.1"
write_entry "$work/gh-assets/v0.1.0/release-index-entry.json" "v0.1.0" "0.1.0" "stable"
cat >"$mock_bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
# Fixture gh: only the three invocations fetch-release-index-entries.sh
# makes. Tag names and paths are argv, never interpolated into sh -c.
cmd="${1:-}"
sub="${2:-}"
shift 2 || true
tag=""
dir=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repo|--limit|--json|--jq|--pattern) shift 2 ;;
    --exclude-drafts) shift ;;
    --dir) dir="$2"; shift 2 ;;
    --*) shift ;;
    *) tag="$1"; shift ;;
  esac
done
assets_root="${HOSERVA_TEST_GH_ASSETS:?}"
if [ "$cmd" = "release" ] && [ "$sub" = "list" ]; then
  printf '%s\n' "v0.1.0" "nightly" "v0.2.0-beta.1"
  exit 0
fi
if [ "$cmd" = "release" ] && [ "$sub" = "view" ]; then
  if [ -f "$assets_root/$tag/release-index-entry.json" ]; then
    printf '%s\n' "SHA256SUMS" "release-index-entry.json"
  else
    printf '%s\n' "SHA256SUMS"
  fi
  exit 0
fi
if [ "$cmd" = "release" ] && [ "$sub" = "download" ]; then
  src="$assets_root/$tag/release-index-entry.json"
  if [ ! -f "$src" ]; then
    echo "gh: no assets matched" >&2
    exit 1
  fi
  cp "$src" "$dir/release-index-entry.json"
  exit 0
fi
echo "gh mock: unexpected invocation $cmd $sub" >&2
exit 1
MOCK
chmod +x "$mock_bin/gh"

fetch_out="$work/fetched"
mkdir -p "$fetch_out"
if ! HOSERVA_TEST_GH_ASSETS="$work/gh-assets" PATH="$mock_bin:$PATH" \
  "$script_dir/fetch-release-index-entries.sh" "$fetch_out"; then
  note "FAIL: fetch-release-index-entries.sh should succeed against the mock gh"
  fail=1
else
  fetched_count="$(find "$fetch_out" -maxdepth 1 -name 'entry-*.json' | wc -l)"
  assert_eq "$(echo "$fetched_count" | tr -d ' ')" "1" "only the release that has the asset is fetched"
  assert_contains "$fetch_out/entry-0001.json" '"tag": "v0.1.0"' "fetched the stable entry"
fi

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
