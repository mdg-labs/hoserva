#!/usr/bin/env bash
# Reads a security-audit report (the fixed format in
# .claude/skills/security-audit/SKILL.md, step 6) and turns it into GitHub
# records: a public issue for each finding that is not withheld and a draft
# security advisory for each one that is (Q91, doc 15 §7).
#
# Usage: audit-report.sh <subcommand> <report> [args...]
#   parse <report>        validate the report and print it as JSON
#   list <report>         print one line per finding: id, severity, public or
#                         withheld, what it was filed as, title
#   file <report> [--epic n] [--confirmed | --dry-run]
#                         file every finding not filed yet; refuses to write
#                         unless --confirmed (the maintainer has seen `list`
#                         and said to go ahead) or --dry-run (writes nothing)
#
# The report is parsed here, deterministically, not by model judgement: a
# report that does not follow the format exactly is refused, naming the
# finding (or the front matter, or the section) at fault, before anything is
# read from or written to GitHub. Every GitHub call goes through
# scripts/gh-rest.sh or scripts/issue-status.sh.
#
# Running it again on the same report creates nothing a second time: a
# finding already filed is found by its marker line, `Audit-finding: <id>`,
# in an issue body or an advisory description, and the id it was filed as is
# written back into the report (`filed:` in the finding's header). A lookup
# that fails is a failure of the run, never "not filed yet".
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
GH_REST="$HERE/gh-rest.sh"
ISSUE_STATUS="$HERE/issue-status.sh"
DEFAULT_EPIC=685

die() { printf 'audit-report: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

PY_SOURCE=$(cat <<'PY_EOF'
import datetime
import json
import os
import re
import sys
import tempfile

AREAS = {
    "area:storage", "area:api", "area:web", "area:cli", "area:shares",
    "area:containers", "area:vm", "area:migration", "area:backup",
    "area:packaging", "area:devenv", "area:site",
}
SEVERITIES = ["critical", "high", "medium", "low", "info"]
FRONT_KEYS = ["run_id", "dev_sha", "scope", "units", "date"]
FINDING_KEYS = ["id", "title", "severity", "type", "area", "safety_critical",
                "withhold", "verdict", "files", "related", "invariant"]
OPTIONAL_KEYS = ["cwe", "filed"]
SECTIONS = ["Summary", "Entry point and attacker", "Verified trace",
            "Impact and preconditions", "Fix direction",
            "Test to write first", "Verifier notes"]
COVERAGE = ["Units", "Checked and found sound", "Unassigned files", "Notes"]
GHSA = re.compile(r"^GHSA(-[2-9cfghjmpqrvwx]{4}){3}$")
ITEM = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/:@+-]*$")
PATH = re.compile(r"^[A-Za-z0-9._@+-]+(/[A-Za-z0-9._@+-]+)*$")
COMMENT = re.compile(r"\s+#.*$")
FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,})(.*)$")
VERDICTS = ["CONFIRMED", "CONFIRMED-WITH-PRECONDITIONS"]
INVARIANT_ROW = re.compile(r"^\|\s*\*\*(T[1-9][0-9]*)\*\*\s*\|")
INVARIANTS = set()


class Refuse(Exception):
    def __init__(self, where, msg):
        super().__init__(f"{where}: {msg}")


def load_invariants(path):
    if not path:
        raise Refuse("threat model", "no path to doc 15 was given")
    try:
        with open(path, encoding="utf-8") as fh:
            lines = fh.read().split("\n")
    except (OSError, UnicodeDecodeError) as e:
        raise Refuse("threat model", f"cannot read {path}: {e}")
    start = [i for i, ln in enumerate(lines) if re.match(r"^## 4\. ", ln)]
    if len(start) != 1:
        raise Refuse("threat model", f"{path} has no single '## 4.' invariants section")
    found = set()
    for ln in lines[start[0] + 1:]:
        if ln.startswith("## "):
            break
        m = INVARIANT_ROW.match(ln)
        if m:
            found.add(m.group(1))
    if not found:
        raise Refuse("threat model", f"{path} defines no invariant in section 4")
    return found


def fence_flags(lines):
    flags, opened = [], None
    for i, ln in enumerate(lines):
        m = FENCE.match(ln)
        if opened is None:
            if m and not (m.group(1)[0] == "`" and "`" in m.group(2)):
                opened = (m.group(1)[0], len(m.group(1)), i)
            flags.append(opened is not None)
        else:
            flags.append(True)
            if (m and m.group(1)[0] == opened[0] and len(m.group(1)) >= opened[1]
                    and m.group(2).strip() == ""):
                opened = None
    return flags, opened


def heading_of(lines, flags, i, prefix):
    if flags[i] or not lines[i].startswith(prefix):
        return None
    return lines[i][len(prefix):]


def parse_block(lines, required, optional, where):
    items = []
    for ln in lines:
        if ln.strip() == "":
            continue
        m = re.match(r"^([a-z_]+): (.*)$", ln)
        if not m:
            raise Refuse(where, f"not a 'key: value' line: {ln[:60]!r}")
        items.append((m.group(1), m.group(2).strip()))
    keys = [k for k, _ in items]
    expected = required + [k for k in optional if k in keys]
    if keys != expected:
        extra = f" (then optionally {', '.join(optional)})" if optional else ""
        raise Refuse(where, f"keys must be {', '.join(required)} in that order{extra}; got {', '.join(keys)}")
    return dict(items)


def scalar(raw, where, key):
    v = COMMENT.sub("", raw).strip()
    if v == "" or v[0] in "\"'[]{}&*!|>%@`#,":
        raise Refuse(where, f"{key}: not a plain value: {raw[:60]!r}")
    return v


def quoted(raw, where, key):
    m = re.match(r'^"((?:[^"\\]|\\["\\])*)"(?:\s+#.*)?$', raw)
    if not m:
        raise Refuse(where, f'{key}: must be one double-quoted string with " and \\ escaped')
    s = re.sub(r"\\([\"\\])", r"\1", m.group(1))
    if s.strip() == "":
        raise Refuse(where, f"{key}: is empty")
    return s


def flow(raw, where, key, item_re=ITEM, non_empty=False):
    v = COMMENT.sub("", raw).strip()
    m = re.match(r"^\[(.*)\]$", v)
    if not m:
        raise Refuse(where, f"{key}: must be a [flow, list]")
    inner = m.group(1).strip()
    items = [] if inner == "" else [x.strip() for x in inner.split(",")]
    for it in items:
        if not item_re.match(it):
            raise Refuse(where, f"{key}: bad list item {it[:60]!r}")
    if non_empty and not items:
        raise Refuse(where, f"{key}: must not be empty")
    return items


def boolean(raw, where, key):
    v = scalar(raw, where, key)
    if v not in ("true", "false"):
        raise Refuse(where, f"{key}: must be true or false")
    return v == "true"


def one_of(raw, where, key, allowed):
    v = scalar(raw, where, key)
    if v not in allowed:
        raise Refuse(where, f"{key}: '{v}' is not one of {', '.join(allowed)}")
    return v


def parse_front(lines):
    if not lines or lines[0] != "---":
        raise Refuse("front matter", "the report must start with a --- line")
    try:
        end = lines.index("---", 1)
    except ValueError:
        raise Refuse("front matter", "no closing --- line")
    w = "front matter"
    raw = parse_block(lines[1:end], FRONT_KEYS, [], w)
    run_id = scalar(raw["run_id"], w, "run_id")
    if not re.match(r"^\d{8}-[0-9a-f]{7}(-\d+)?$", run_id):
        raise Refuse(w, f"run_id: '{run_id}' is not <yyyymmdd>-<sha7>[-<n>]")
    sha = scalar(raw["dev_sha"], w, "dev_sha")
    if not re.match(r"^[0-9a-f]{40}$", sha):
        raise Refuse(w, "dev_sha: must be a full 40-character SHA")
    if run_id.split("-")[1] != sha[:7]:
        raise Refuse(w, "run_id and dev_sha name different commits")
    scope_raw = COMMENT.sub("", raw["scope"]).strip()
    scope = "whole codebase" if scope_raw == "whole codebase" else flow(raw["scope"], w, "scope", non_empty=True)
    units = flow(raw["units"], w, "units", non_empty=True)
    date = scalar(raw["date"], w, "date")
    try:
        datetime.date.fromisoformat(date)
    except ValueError:
        raise Refuse(w, f"date: '{date}' is not an ISO date")
    return end, {"run_id": run_id, "dev_sha": sha, "scope": scope, "units": units, "date": date}


def split_sections(lines, flags, start, stop, where, names):
    """Split lines[start:stop] on '### ' headings (outside fences) that must
    be exactly `names`, in order; returns {name: content}, content stripped."""
    marks = []
    for i in range(start, stop):
        h = heading_of(lines, flags, i, "### ")
        if h is not None:
            marks.append((i, h.strip()))
        elif not flags[i] and lines[i].startswith("###") and not lines[i].startswith("####"):
            raise Refuse(where, f"malformed ### heading: {lines[i][:60]!r}")
    got = [h for _, h in marks]
    if got != names:
        raise Refuse(where, "the ### headings must be exactly "
                            f"{', '.join(names)} in that order; got {', '.join(got) or 'none'}")
    if any(lines[i].strip() for i in range(start, marks[0][0])):
        raise Refuse(where, "text before the first ### heading")
    out = {}
    for n, (i, h) in enumerate(marks):
        j = marks[n + 1][0] if n + 1 < len(marks) else stop
        text = "\n".join(lines[i + 1:j]).strip("\n").strip()
        out[h] = text
    return out


def parse_finding(lines, flags, head, stop, run_id):
    m = re.match(r"^## (SA-\S*)(?: — (.*))?$", lines[head])
    if not m:
        raise Refuse(lines[head][3:].split(" ")[0], "the heading must read '## <id> — <title>'")
    fid = m.group(1)
    if not re.match(r"^SA-" + re.escape(run_id) + r"-\d{2,}$", fid):
        raise Refuse(fid, f"the id must be SA-{run_id}-<nn>")
    if m.group(2) is None or m.group(2).strip() == "":
        raise Refuse(fid, "the heading must read '## <id> — <title>'")
    i = head + 1
    while i < stop and lines[i].strip() == "":
        i += 1
    o = FENCE.match(lines[i]) if i < stop else None
    if not o or o.group(1)[0] != "`" or o.group(2).strip() != "yaml":
        raise Refuse(fid, "the heading must be followed by a fenced yaml block")
    j = i + 1
    while j < stop and not (FENCE.match(lines[j]) and FENCE.match(lines[j]).group(1)[0] == "`"
                            and len(FENCE.match(lines[j]).group(1)) >= len(o.group(1))
                            and FENCE.match(lines[j]).group(2).strip() == ""):
        j += 1
    if j >= stop:
        raise Refuse(fid, "the yaml block is not closed")
    raw = parse_block(lines[i + 1:j], FINDING_KEYS, OPTIONAL_KEYS, fid)
    f = {"id": fid}
    if scalar(raw["id"], fid, "id") != fid:
        raise Refuse(fid, "id: does not match the heading")
    f["title"] = quoted(raw["title"], fid, "title")
    if f["title"] != m.group(2).strip():
        raise Refuse(fid, "title: does not match the heading")
    f["severity"] = one_of(raw["severity"], fid, "severity", SEVERITIES)
    f["type"] = one_of(raw["type"], fid, "type", ["bug", "chore", "docs"])
    f["area"] = one_of(raw["area"], fid, "area", sorted(AREAS) + ["none"])
    f["safety_critical"] = boolean(raw["safety_critical"], fid, "safety_critical")
    f["withhold"] = boolean(raw["withhold"], fid, "withhold")
    f["verdict"] = one_of(raw["verdict"], fid, "verdict", VERDICTS)
    f["files"] = flow(raw["files"], fid, "files", PATH, non_empty=True)
    if any(".." in p.split("/") for p in f["files"]):
        raise Refuse(fid, "files: a path may not contain ..")
    f["related"] = flow(raw["related"], fid, "related", re.compile(r"^SA-[A-Za-z0-9-]+$"))
    inv = scalar(raw["invariant"], fid, "invariant")
    if inv != "none" and inv not in INVARIANTS:
        raise Refuse(fid, f"invariant: {inv[:60]!r} is not none or an invariant doc 15 section 4 defines")
    f["invariant"] = inv
    f["cwe"] = flow(raw["cwe"], fid, "cwe", re.compile(r"^CWE-[0-9]+$")) if "cwe" in raw else []
    f["filed"] = None
    if "filed" in raw:
        v = raw["filed"]
        if v.startswith('"'):
            v = quoted(v, fid, "filed")
        else:
            v = scalar(v, fid, "filed")
        if f["withhold"]:
            if not GHSA.match(v):
                raise Refuse(fid, "filed: a withheld finding is filed as a GHSA id")
        elif not re.match(r"^#[0-9]+$", v):
            raise Refuse(fid, "filed: a public finding is filed as an issue number like \"#12\"")
        f["filed"] = v
    f["sections"] = split_sections(lines, flags, j + 1, stop, fid, SECTIONS)
    for name, text in f["sections"].items():
        if text == "":
            raise Refuse(fid, f"the '{name}' section is empty")
    f["text"] = "\n".join(lines[head:stop]).strip("\n")
    return f, (i + 1, j)


def parse_report(text):
    if "\r" in text:
        raise Refuse("report", "must use LF line endings")
    lines = text.split("\n")
    flags, opened = fence_flags(lines)
    if opened is not None:
        near = [lines[k][3:].split(" ")[0] for k in range(opened[2]) if lines[k].startswith("## SA-")]
        raise Refuse(near[-1] if near else "report", "a code fence is never closed")
    fm_end, front = parse_front(lines)
    tops = []
    for i in range(fm_end + 1, len(lines)):
        h = heading_of(lines, flags, i, "## ")
        if h is not None:
            tops.append((i, h.strip()))
        elif not flags[i] and lines[i].startswith("##") and not lines[i].startswith("###"):
            raise Refuse("report", f"malformed ## heading: {lines[i][:60]!r}")
    if any(lines[k].strip() for k in range(fm_end + 1, tops[0][0] if tops else len(lines))):
        raise Refuse("report", "text between the front matter and '## Summary'")
    names = [t for _, t in tops]
    tail = ["Refuted candidates", "Coverage", "Threat-model gaps"]
    if not names or names[0] != "Summary":
        raise Refuse("report", "the first section must be '## Summary'")
    if names[-3:] != tail:
        raise Refuse("report", "the last sections must be ## Refuted candidates, ## Coverage and ## Threat-model gaps")
    middle = tops[1:-3]
    for _, t in middle:
        if not t.startswith("SA-"):
            raise Refuse("report", f"unexpected section '## {t}' among the findings")
    bounds = [i for i, _ in tops] + [len(lines)]
    findings, meta, seen = [], {}, set()
    for n in range(1, len(tops) - 3):
        f, hdr = parse_finding(lines, flags, tops[n][0], bounds[n + 1], front["run_id"])
        if f["id"] in seen:
            raise Refuse(f["id"], "appears twice")
        seen.add(f["id"])
        findings.append(f)
        meta[f["id"]] = hdr
    by_id = {f["id"]: f for f in findings}
    for f in findings:
        if f["severity"] in ("critical", "high") and not f["withhold"]:
            raise Refuse(f["id"], f"a {f['severity']} finding must be withheld")
        for r in f["related"]:
            if r == f["id"] or r not in by_id:
                raise Refuse(f["id"], f"related: '{r}' is not another finding in this report")
            if by_id[r]["withhold"] != f["withhold"]:
                pub = f["id"] if not f["withhold"] else r
                raise Refuse(pub, "shares a root cause with a withheld finding and must be withheld too")
    withheld_ids = [f["id"] for f in findings if f["withhold"]]
    for f in findings:
        if f["withhold"]:
            continue
        blob = f["title"] + "\n" + f["text"]
        for w in withheld_ids:
            if w in blob:
                raise Refuse(f["id"], "a public finding mentions a withheld finding")
    summary = "\n".join(lines[tops[0][0] + 1:tops[1][0] if len(tops) > 1 else len(lines)]).split("\n")
    check_summary(summary, findings)
    for n, name in enumerate(tail):
        k = len(tops) - 3 + n
        body = "\n".join(lines[tops[k][0] + 1:bounds[k + 1]]).strip()
        if body == "":
            raise Refuse("report", f"the '## {name}' section is empty")
        if name == "Coverage":
            split_sections(lines, flags, tops[k][0] + 1, bounds[k + 1], "## Coverage", COVERAGE)
    return {"front": front, "findings": findings}, meta, lines


def check_summary(block, findings):
    rows = [ln for ln in block if ln.strip()]
    if not rows:
        raise Refuse("## Summary", "the totals line is missing")
    m = re.match(r"^(\d+) findings(?:: (\d+) critical, (\d+) high, (\d+) medium, (\d+) low, (\d+) info; (\d+) candidates refuted)?$", rows[0])
    if not m:
        raise Refuse("## Summary", f"not a totals line: {rows[0][:80]!r}")
    if int(m.group(1)) != len(findings):
        raise Refuse("## Summary", f"the totals line says {m.group(1)} findings, the report holds {len(findings)}")
    if m.group(2) is not None:
        for sev, got in zip(SEVERITIES, m.groups()[1:6]):
            n = sum(1 for f in findings if f["severity"] == sev)
            if int(got) != n:
                raise Refuse("## Summary", f"the totals line says {got} {sev}, the findings hold {n}")
    if len(rows) < 2 or not re.match(r"^\|\s*id\s*\|\s*severity\s*\|\s*title\s*\|\s*verdict\s*\|\s*withhold\s*\|$", rows[1]):
        raise Refuse("## Summary", "the table header must be: | id | severity | title | verdict | withhold |")
    if len(rows) < 3 or not re.match(r"^\|(\s*:?-+:?\s*\|){5}$", rows[2]):
        raise Refuse("## Summary", "the table's separator row is missing")
    data = rows[3:]
    if len(data) != len(findings):
        raise Refuse("## Summary", f"the table has {len(data)} rows for {len(findings)} findings")
    pat = re.compile(r"^\|\s*(SA-[^|\s]+)\s*\|\s*([a-z]+)\s*\|\s*(.*?)\s*\|\s*(CONFIRMED(?:-WITH-PRECONDITIONS)?)\s*\|\s*(true|false)\s*\|$")
    for row, f in zip(data, findings):
        c = pat.match(row)
        if not c:
            raise Refuse(f["id"], f"its row in the ## Summary table is malformed: {row[:80]!r}")
        if (c.group(1), c.group(2), c.group(4), c.group(5) == "true") != (f["id"], f["severity"], f["verdict"], f["withhold"]):
            raise Refuse(f["id"], "its row in the ## Summary table disagrees with its section")


def load(path):
    try:
        with open(path, encoding="utf-8") as fh:
            return fh.read()
    except (OSError, UnicodeDecodeError) as e:
        raise Refuse("report", f"cannot read {path}: {e}")


def find(report, fid):
    for f in report["findings"]:
        if f["id"] == fid:
            return f
    raise Refuse("report", f"no finding {fid}")


def issue_body(report, f):
    s = f["sections"]
    files = ", ".join(f"`{p}`" for p in f["files"])
    refs = ["- `docs/internal/15-threat-model.md` §6 (severity rubric — rated **%s**) and §7 (disclosure split); Q91." % f["severity"]]
    if f["invariant"] != "none":
        refs.append(f"- Invariant {f['invariant']} (doc 15 §4).")
    out = [
        "## Summary", "", s["Summary"], "",
        "## Design references", "", *refs, "",
        "## Root cause / relevant code", "",
        "**Entry point and attacker**", "", s["Entry point and attacker"], "",
        "**Verified trace**", "", s["Verified trace"], "",
        "**Impact and preconditions**", "", s["Impact and preconditions"], "",
        "**Verifier notes**", "", s["Verifier notes"], "",
        "## Proposed approach", "", s["Fix direction"], "",
        "## Acceptance criteria", "",
        "- [ ] The test described below is written before the fix, fails without it and passes with it.",
        "- [ ] The weakness described under \"Root cause / relevant code\" no longer holds on the production build and defaults; the fix follows \"Proposed approach\" or says why it differs.",
        "- [ ] Reachable via: the entry point named under \"Root cause / relevant code\" — the fix is checked there, not only in a helper.",
        "", "**Test to write first**", "", s["Test to write first"], "",
        "## Out of scope", "",
        "- The other findings of the same audit run; each is filed on its own.",
        "- Hardening beyond the fix direction above.", "",
        "## Scope hint", "", files, "",
        f"Audit run `{report['front']['run_id']}` on `dev` at `{report['front']['dev_sha']}`.", "",
        f"Audit-finding: {f['id']}", "",
    ]
    return "\n".join(out)


def advisory_body(report, f):
    fr = report["front"]
    return (f"Audit-finding: {f['id']}\n"
            f"Audit-run: {fr['run_id']} (dev {fr['dev_sha']})\n\n{f['text']}\n")


def record(path, fid, value):
    text = load(path)
    report, meta, lines = parse_report(text)
    f = find(report, fid)
    if f["withhold"]:
        if not GHSA.match(value):
            raise Refuse(fid, "a withheld finding is recorded as a GHSA id")
        shown = value
    else:
        if not re.match(r"^#[0-9]+$", value):
            raise Refuse(fid, "a public finding is recorded as an issue number like #12")
        shown = f'"{value}"'
    start, end = meta[fid]
    block = [ln for ln in lines[start:end] if not ln.startswith("filed: ")]
    new = lines[:start] + block + [f"filed: {shown}"] + lines[end:]
    out = "\n".join(new)
    parse_report(out)
    d = os.path.dirname(os.path.abspath(path))
    fd, tmp = tempfile.mkstemp(dir=d, prefix=".audit-report-")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(out)
        os.chmod(tmp, os.stat(path).st_mode & 0o7777)
        os.replace(tmp, path)
    except BaseException:
        if os.path.exists(tmp):
            os.unlink(tmp)
        raise


def main(argv):
    global INVARIANTS
    mode, path = argv[0], argv[1]
    try:
        INVARIANTS = load_invariants(os.environ.get("AUDIT_THREAT_MODEL", ""))
        if mode == "record":
            record(path, argv[2], argv[3])
            return 0
        report, _, _ = parse_report(load(path))
        if mode == "parse":
            json.dump(report, sys.stdout, indent=1)
            sys.stdout.write("\n")
        elif mode == "issue-body":
            sys.stdout.write(issue_body(report, find(report, argv[2])))
        elif mode == "advisory-body":
            sys.stdout.write(advisory_body(report, find(report, argv[2])))
        else:
            raise Refuse("usage", f"unknown mode {mode}")
    except Refuse as e:
        sys.stderr.write(f"audit-report: {e}\n")
        return 1
    except (KeyError, IndexError, AttributeError, ValueError, TypeError) as e:
        sys.stderr.write(f"audit-report: report: could not be parsed ({type(e).__name__}); it does not follow the format\n")
        return 1
    return 0


sys.exit(main(sys.argv[1:]))
PY_EOF
)

# Doc 15 is read from the repository this script sits in, whatever the caller's
# working directory; the override exists for the tests.
THREAT_MODEL=${AUDIT_THREAT_MODEL:-$HERE/../docs/internal/15-threat-model.md}

py() { AUDIT_THREAT_MODEL=$THREAT_MODEL python3 -I -c "$PY_SOURCE" "$@"; }

usage() {
  cat <<'USAGE_EOF'
Usage: audit-report.sh <subcommand> <report> [args...]
  parse <report>
  list <report>
  file <report> [--epic n] [--confirmed | --dry-run]
USAGE_EOF
}

need_report() {
  [[ $# -ge 1 && -n $1 ]] || die "$2: a report path is required"
  [[ -f $1 ]] || die "$2: not a file: $1"
}

cmd_parse() {
  [[ $# -eq 1 ]] || die "usage: parse <report>"
  need_report "$1" parse
  py parse "$1"
}

print_list() {
  local json=$1
  jq -r '.findings[] | [.id, .severity, (if .withhold then "withheld" else "public" end), (.filed // "-"), .title] | @tsv' <<<"$json"
}

cmd_list() {
  [[ $# -eq 1 ]] || die "usage: list <report>"
  need_report "$1" list
  local json
  json=$(py parse "$1") || exit 1
  printf 'id\tseverity\tkind\tfiled\ttitle\n'
  print_list "$json"
}

# Prints the number of the issue carrying the marker for $1, nothing when
# there is none. A failed lookup returns non-zero and prints nothing: it must
# never read as "not filed yet".
find_issue() {
  local id=$1 out
  out=$("$GH_REST" issue-search "Audit-finding: $id" --state all) || return 1
  jq -r --arg id "$id" '
    [ .[] | select((.body // "") | split("\n") | map(rtrimstr("\r")) | any(. == "Audit-finding: " + $id)) | .number ]
    | sort | .[0] // empty' <<<"$out"
}

find_advisory() {
  local id=$1 out
  out=$("$GH_REST" advisory-list --state draft) || return 1
  jq -r --arg id "$id" '
    [ .[] | select((.description // "") | split("\n") | map(rtrimstr("\r")) | any(. == "Audit-finding: " + $id)) | .ghsa_id ]
    | sort | .[0] // empty' <<<"$out"
}

# Puts the issue under the epic and sets it ready — each only when it is not
# already so, because this also runs for an issue an earlier, interrupted run
# created, and must not move one that work has since started on.
finish_issue() {
  local num=$1 epic=$2 info parent state statuses other
  info=$("$GH_REST" issue-view "$num" --jq '.state, (.labels[].name)') \
    || { say "audit-report: could not read #$num" >&2; return 1; }
  state=$(head -n1 <<<"$info")
  if [[ $state != open ]]; then
    say "  #$num is $state; left as it is"
    return 0
  fi
  parent=$("$GH_REST" parent "$num" --jq '.number') \
    || { say "audit-report: could not read the parent of #$num" >&2; return 1; }
  if [[ -z $parent ]]; then
    "$GH_REST" add-sub-issue "$epic" "$num" || return 1
    say "  #$num added under epic #$epic"
  elif [[ $parent != "$epic" ]]; then
    say "  #$num is under #$parent, not #$epic; left as it is"
  fi
  statuses=$(tail -n +2 <<<"$info" | { grep '^status:' || true; })
  other=$(grep -vx 'status:new' <<<"$statuses" || true)
  if [[ -n $other ]]; then
    say "  #$num already has a status beyond new; left as it is"
  else
    "$ISSUE_STATUS" "$num" ready >/dev/null || return 1
    say "  #$num set ready"
  fi
}

file_public() {
  local id=$1 filed=$2 report=$3 epic=$4 milestone=$5 dry=$6 tmp=$7 num="" title labels_out
  if [[ $filed == '#'* ]]; then
    num=${filed#\#}
    say "$id: already filed as #$num"
  else
    num=$(find_issue "$id") || { say "audit-report: $id: could not look for an existing issue" >&2; return 1; }
    if [[ -n $num ]]; then
      say "$id: already filed as #$num"
      (( dry )) || py record "$report" "$id" "#$num" || return 1
    else
      title=$(jq -r --arg id "$id" '.findings[] | select(.id == $id) | .title' <<<"$json") || return 1
      labels_out=$(jq -r --arg id "$id" '
        .findings[] | select(.id == $id)
        | [.type] + (if .area != "none" then [.area] else [] end) + ["security"]
          + (if .safety_critical then ["safety-critical"] else [] end) | .[]' <<<"$json") || return 1
      mapfile -t labels <<<"$labels_out"
      if (( dry )); then
        say "$id: would create an issue with labels ${labels[*]}, milestone '$milestone', under epic #$epic, set ready"
        return 0
      fi
      py issue-body "$report" "$id" >"$tmp/body.md" || return 1
      local args=(--title "$title" --body-file "$tmp/body.md" --milestone "$milestone") label out
      for label in "${labels[@]}"; do args+=(--label "$label"); done
      out=$("$GH_REST" issue-create "${args[@]}") || { say "audit-report: $id: issue-create failed" >&2; return 1; }
      num=$(jq -r '.number // empty' <<<"$out")
      [[ $num =~ ^[0-9]+$ ]] || { say "audit-report: $id: issue-create gave no issue number; look for the marker 'Audit-finding: $id' before re-running" >&2; return 1; }
      say "$id: created #$num"
      py record "$report" "$id" "#$num" || return 1
    fi
  fi
  if (( dry )); then
    say "  would check #$num is under epic #$epic and ready"
    return 0
  fi
  finish_issue "$num" "$epic"
}

file_withheld() {
  local id=$1 filed=$2 report=$3 dry=$4 tmp=$5 ghsa="" sev
  if [[ -n $filed ]]; then
    say "$id: already filed as $filed"
    return 0
  fi
  ghsa=$(find_advisory "$id") || { say "audit-report: $id: could not look for an existing advisory" >&2; return 1; }
  if [[ -n $ghsa ]]; then
    say "$id: already filed as $ghsa"
    (( dry )) || py record "$report" "$id" "$ghsa" || return 1
    return 0
  fi
  sev=$(jq -r --arg id "$id" '.findings[] | select(.id == $id) | if .severity == "info" then "low" else .severity end' <<<"$json") || return 1
  local args=(--severity "$sev") cwe cwes summary
  cwes=$(jq -r --arg id "$id" '.findings[] | select(.id == $id) | .cwe[]' <<<"$json") || return 1
  while IFS= read -r cwe; do
    [[ -n $cwe ]] && args+=(--cwe "$cwe")
  done <<<"$cwes"
  summary=$(jq -r --arg id "$id" '.findings[] | select(.id == $id) | .title' <<<"$json") || return 1
  py advisory-body "$report" "$id" >"$tmp/advisory.md" || return 1
  args+=(--summary "$summary" --description-file "$tmp/advisory.md")
  if (( dry )); then
    "$GH_REST" advisory-create "${args[@]}" --dry-run >/dev/null || return 1
    say "$id: would create a draft advisory, severity $sev"
    return 0
  fi
  local out
  out=$("$GH_REST" advisory-create "${args[@]}") || { say "audit-report: $id: advisory-create failed" >&2; return 1; }
  ghsa=$(jq -r '.ghsa_id // empty' <<<"$out")
  [[ $ghsa =~ ^GHSA- ]] || { say "audit-report: $id: advisory-create gave no GHSA id; look for the marker 'Audit-finding: $id' before re-running" >&2; return 1; }
  say "$id: created draft advisory $ghsa"
  py record "$report" "$id" "$ghsa"
}

cmd_file() {
  local report="" epic=$DEFAULT_EPIC confirmed=0 dry=0
  while [[ $# -gt 0 ]]; do
    case $1 in
      --epic) [[ $# -ge 2 ]] || die "file: --epic needs a value"; epic=$2; shift 2 ;;
      --confirmed) confirmed=1; shift ;;
      --dry-run) dry=1; shift ;;
      -*) die "file: unrecognized argument: $1" ;;
      *) [[ -z $report ]] || die "file: unrecognized argument: $1"; report=$1; shift ;;
    esac
  done
  need_report "$report" file
  [[ $epic =~ ^[0-9]+$ ]] || die "file: --epic must be an issue number, got '$epic'"
  json=$(py parse "$report") || exit 1

  printf 'id\tseverity\tkind\tfiled\ttitle\n'
  print_list "$json"
  if (( ! dry && ! confirmed )); then
    die "file: refusing to write: show the list above to the maintainer and re-run with --confirmed once they say to file it (or with --dry-run to see what would happen)"
  fi

  local rows
  rows=$(jq -r '.findings[] | [.id, .withhold, (.filed // "")] | join("|")' <<<"$json")
  [[ -n $rows ]] || { say "no findings: nothing to file"; return 0; }

  local milestone=""
  if jq -e 'any(.findings[]; .withhold | not)' <<<"$json" >/dev/null; then
    local epic_json
    epic_json=$("$GH_REST" issue-view "$epic") || die "file: could not read epic #$epic"
    jq -e '.state == "open" and any(.labels[]; .name == "epic")' <<<"$epic_json" >/dev/null \
      || die "file: #$epic is not an open epic"
    milestone=$(jq -r '.milestone.title // empty' <<<"$epic_json")
    [[ -n $milestone ]] || die "file: epic #$epic has no milestone"
  fi

  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT

  local id withhold filed
  while IFS='|' read -r id withhold filed; do
    if [[ $withhold == true ]]; then
      file_withheld "$id" "$filed" "$report" "$dry" "$tmp" || die "stopped at $id; re-run to continue — nothing already filed is filed again"
    else
      file_public "$id" "$filed" "$report" "$epic" "$milestone" "$dry" "$tmp" || die "stopped at $id; re-run to continue — nothing already filed is filed again"
    fi
  done <<<"$rows"
}

main() {
  [[ $# -ge 1 ]] || { usage >&2; exit 1; }
  local sub=$1; shift
  case $sub in
    parse) cmd_parse "$@" ;;
    list) cmd_list "$@" ;;
    file) cmd_file "$@" ;;
    help | -h | --help) usage ;;
    *) die "unknown subcommand: $sub" ;;
  esac
}

main "$@"
