#!/usr/bin/env bash
# catalog-snapshot.sh's refusals, against a fake `curl` and throwaway pin and
# embed directories (CATALOG_PIN_FILE, CATALOG_SNAPSHOT_DIR): a failed
# download, a pin mismatch, a bad signature and a malformed pin each fail the
# script and leave nothing in the embed directory, even over a cached copy
# that did not match; a cached copy that matches the pin is kept without any
# download. Needs the real snapshot, so `make catalog-snapshot-test` runs
# `make catalog-snapshot` first. Never reaches the network.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/../.." && pwd)"
snapshot="$repo_root/internal/template/snapshot"

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT

mkdir -p "$work/bin" "$work/served"
cat >"$work/bin/curl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
out=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) out="$2"; shift 2 ;;
    --max-time | --retry | --proto | --proto-redir) shift 2 ;;
    --*) shift ;;
    *) url="$1"; shift ;;
  esac
done
echo "$url" >>"$FAKE_CURL_LOG"
src="$FAKE_CURL_SERVED/$(basename -- "$url")"
[ -f "$src" ] || exit 22
cp -- "$src" "$out"
FAKE
chmod +x "$work/bin/curl"
export PATH="$work/bin:$PATH"
export FAKE_CURL_LOG="$work/curl.log"
export FAKE_CURL_SERVED="$work/served"

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

# expect_refusal <name> <pin> <dest> <text>: the script must fail, leave nothing
# in the dest dir, and print the expected text.
expect_refusal() {
  local name="$1" pin="$2" dest="$3" want="$4" out left
  : >"$FAKE_CURL_LOG"
  if out="$(CATALOG_PIN_FILE="$pin" CATALOG_SNAPSHOT_DIR="$dest" "$script_dir/catalog-snapshot.sh" 2>&1)"; then
    fail "$name: the script succeeded"
    return
  fi
  left="$(find "$dest" -mindepth 1 -maxdepth 1)"
  if [ -n "$left" ]; then
    fail "$name: left ${left//$'\n'/ } in the embed directory"
  fi
  if ! grep -q -- "$want" <<<"$out"; then
    fail "$name: output does not mention '$want': $out"
  fi
}

printf 'not a catalog' >"$work/served/catalog.tar.zst"
sum="$(sha256sum -- "$work/served/catalog.tar.zst" | awk '{ print $1 }')"
printf 'serial=123\nsha256=%s\n' "$sum" >"$work/pin.ok"

# A failed download: the sig is not served.
mkdir "$work/d1"
expect_refusal "failed download" "$work/pin.ok" "$work/d1" "downloading"

# The download is the pinned release, never the refresh URL.
if ! grep -q -- 'https://github.com/mdg-labs/hoserva-catalog/releases/download/serial-123/catalog.tar.zst$' "$FAKE_CURL_LOG"; then
  fail "the archive was not fetched from the pinned release: $(cat "$FAKE_CURL_LOG")"
fi
if grep -q 'catalog.hoserva.dev' "$FAKE_CURL_LOG"; then
  fail "the refresh URL was used at build time"
fi

# A bad signature over an archive whose SHA-256 matches the pin.
head -c 64 /dev/zero >"$work/served/catalog.tar.zst.sig"
mkdir "$work/d2"
expect_refusal "bad signature" "$work/pin.ok" "$work/d2" "does not verify"

# A pin mismatch.
printf 'serial=123\nsha256=%s\n' "$(printf '0%.0s' {1..64})" >"$work/pin.mismatch"
mkdir "$work/d3"
expect_refusal "pin mismatch" "$work/pin.mismatch" "$work/d3" "SHA-256"

# A cached copy that does not match the pin does not survive a failed fetch.
mkdir "$work/d4"
printf 'old' >"$work/d4/catalog.tar.zst"
printf 'old' >"$work/d4/catalog.tar.zst.sig"
rm -f -- "$work/served/catalog.tar.zst.sig"
expect_refusal "stale cache over a failed download" "$work/pin.ok" "$work/d4" "downloading"

# A malformed pin.
printf 'serial=1\nserial=2\nsha256=%s\n' "$sum" >"$work/pin.dup"
printf 'serial=abc\nsha256=%s\n' "$sum" >"$work/pin.serial"
printf 'serial=123\nsha256=ABC\n' >"$work/pin.sha"
for name in dup serial sha; do
  mkdir "$work/m-$name"
  expect_refusal "malformed pin ($name)" "$work/pin.$name" "$work/m-$name" "catalog-snapshot:"
done

# A cached copy that matches the pin and verifies is kept without a download.
if [ ! -f "$snapshot/catalog.tar.zst" ] || [ ! -f "$snapshot/catalog.tar.zst.sig" ]; then
  fail "the real snapshot is missing — run make catalog-snapshot first"
else
  mkdir "$work/d5"
  cp -- "$snapshot/catalog.tar.zst" "$snapshot/catalog.tar.zst.sig" "$work/d5/"
  rm -f -- "$work/served"/*
  : >"$FAKE_CURL_LOG"
  if ! CATALOG_SNAPSHOT_DIR="$work/d5" "$script_dir/catalog-snapshot.sh" >/dev/null 2>&1; then
    fail "a cached copy matching the pin was refused"
  fi
  if [ -s "$FAKE_CURL_LOG" ]; then
    fail "a cached copy matching the pin was downloaded again: $(cat "$FAKE_CURL_LOG")"
  fi
  if ! cmp -s "$snapshot/catalog.tar.zst" "$work/d5/catalog.tar.zst"; then
    fail "a cached copy matching the pin was replaced"
  fi
fi

if [ "$failures" -ne 0 ]; then
  echo "test-catalog-snapshot: $failures failure(s)" >&2
  exit 1
fi
echo "test-catalog-snapshot: ok"
