#!/usr/bin/env bash
# No outbound request may be baked into the built web app (Q49). Usage:
# check-web-outbound.sh [dist-dir]   (default: web/dist next to this repo)
#
# SVG/XML namespace URIs and error-message URLs in minified library code
# (react-router, Base UI, React, Recharts' prop-types, react-router's
# useSearchParams IE11 polyfill pointer) are not requests and are exempted by
# name. So are the RFC 2606 / RFC 6761 reserved documentation names
# (example.com/.net/.org and their subdomains, and the .example, .test and
# .invalid TLDs): they can never resolve, so a placeholder address in UI copy
# is not a request. Every entry is anchored on the URL's host (and path, where
# one is named), so `example.com.evil.io`, `notexample.com`,
# `example.com@evil.io` and `evil.io/?x=localhost` still fail. Anything else
# found here is a real regression.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
dist="${1:-$script_dir/../../web/dist}"

label='[A-Za-z0-9-]+'
reserved="^https?://(${label}\\.)*(example\\.(com|net|org)|example|test|invalid)\\.?(:[0-9]+)?([/?#].*)?\$"
named="^https?://(www\\.w3\\.org|react\\.dev/errors|base-ui\\.com/production-error|react\\.i18next\\.com|reactrouter\\.com|localhost(:[0-9]+)?)([/?#].*)?\$"
allowed="(${named}|^https?://github\\.com/ungap/url-search-params(\\.|[?#].*)?\$|^https?://fb\\.me/use-check-prop-types([?#].*)?\$|${reserved})"

if [ ! -d "$dist" ]; then
  echo "check-web-outbound: $dist is not a directory — build the web app first" >&2
  exit 2
fi

rc=0
urls="$(grep -rEho "https?://[^\"'\\) ]+" "$dist")" || rc=$?
if [ "$rc" -gt 1 ]; then
  echo "check-web-outbound: could not scan $dist" >&2
  exit 2
fi

found=""
if [ -n "$urls" ]; then
  rc=0
  found="$(printf '%s\n' "$urls" | grep -Ev "$allowed")" || rc=$?
  if [ "$rc" -gt 1 ]; then
    echo "check-web-outbound: could not filter the URLs found in $dist" >&2
    exit 2
  fi
fi

if [ -n "$found" ]; then
  echo "$found"
  echo "web: the build embeds an outbound request Hoserva did not intend (Q49)" >&2
  exit 1
fi
