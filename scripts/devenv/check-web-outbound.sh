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
# `example.com@evil.io` and `evil.io/?x=localhost` still fail. A URL whose
# whole host is a template interpolation (`http://${host}:${port}`, a link to
# a container on the user's own server) embeds no host. The first scan cuts
# such a URL short inside the interpolation, so it is read again up to the
# template literal's closing backtick and accepted only when its
# interpolations close and nothing but a port (or another interpolation)
# follows the host: `${h}.evil.io`, `${f()}.evil.io` and `${h}@evil.io` still
# fail. Anything else found here is a real regression.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
dist="${1:-$script_dir/../../web/dist}"

label='[A-Za-z0-9-]+'
reserved="^https?://(${label}\\.)*(example\\.(com|net|org)|example|test|invalid)\\.?(:[0-9]+)?([/?#].*)?\$"
named="^https?://(www\\.w3\\.org|react\\.dev/errors|base-ui\\.com/production-error|react\\.i18next\\.com|reactrouter\\.com|localhost(:[0-9]+)?)([/?#].*)?\$"
interpolated='^https?://\$\{'
allowed="(${named}|${interpolated}|^https?://github\\.com/ungap/url-search-params(\\.|[?#].*)?\$|^https?://fb\\.me/use-check-prop-types([?#].*)?\$|${reserved})"

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

rc=0
templated="$(grep -rEho 'https?://\$\{[^`]*' "$dist")" || rc=$?
if [ "$rc" -gt 1 ]; then
  echo "check-web-outbound: could not scan $dist for interpolated URLs" >&2
  exit 2
fi
if [ -n "$templated" ]; then
  rc=0
  # Each interpolation becomes a backtick, which the capture cannot contain.
  bad="$(printf '%s\n' "$templated" | awk '
    {
      s = $0
      sub(/^https?:\/\//, "", s)
      out = ""
      depth = 0
      for (i = 1; i <= length(s); i++) {
        c = substr(s, i, 1)
        if (depth == 0 && substr(s, i, 2) == "${") {
          depth = 1
          i++
          out = out "`"
        } else if (depth > 0) {
          if (c == "{") depth++
          else if (c == "}") depth--
        } else {
          out = out c
        }
      }
      authority = out
      if (match(out, /[\/?#]/)) authority = substr(out, 1, RSTART - 1)
      if (depth != 0 || authority !~ /^`(:([0-9]+|`))?$/) print $0
    }')" || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "check-web-outbound: could not check the interpolated URLs found in $dist" >&2
    exit 2
  fi
  if [ -n "$bad" ]; then
    found="${found:+$found
}$bad"
  fi
fi

if [ -n "$found" ]; then
  echo "$found"
  echo "web: the build embeds an outbound request Hoserva did not intend (Q49)" >&2
  exit 1
fi
