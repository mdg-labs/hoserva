#!/usr/bin/env bash
# Verify every non-merge commit in (base, head] carries a Signed-off-by
# trailer matching its own author's email (CONTRIBUTING.md, doc 13 Q2).
#
# Merge commits are skipped: they carry no diff of their own, and the
# commits they merge are checked individually. Trailer parsing uses git's
# own `%(trailers:...)` format, not a message-body regex, so ordering or
# wrapping in the rest of the message can't produce a false negative.
set -euo pipefail

die() { printf '%s\n' "$*" >&2; exit 1; }

[ $# -eq 2 ] || die "usage: $0 <base-sha> <head-sha>"
base=$1
head=$2

# A brand-new branch's push carries an all-zero `before` SHA (no prior
# commit to diff against) — there is no real base to range from, so check
# every commit reachable from head instead of guessing at head~1, which
# would silently skip everything but the pushed tip.
case "$base" in
  0000000000000000000000000000000000000000|"") range="$head" ;;
  *)
    git rev-parse --verify "$base" >/dev/null 2>&1 || die "check-dco: base '$base' is not a known commit (shallow clone? fetch-depth: 0 is required)"
    range="$base..$head"
    ;;
esac

fail=0
while read -r sha; do
  author_email=$(git log -1 --format='%ae' "$sha")
  signoffs=$(git log -1 --format='%(trailers:key=Signed-off-by,valueonly)' "$sha")
  if [ -z "$signoffs" ]; then
    printf 'check-dco: %s (%s) has no Signed-off-by trailer\n' "$sha" "$author_email" >&2
    fail=1
    continue
  fi
  if ! printf '%s\n' "$signoffs" | grep -qiF "<$author_email>"; then
    printf 'check-dco: %s (%s) has a Signed-off-by trailer that does not match its author email\n' "$sha" "$author_email" >&2
    fail=1
  fi
done < <(git rev-list --no-merges "$range")

exit "$fail"
