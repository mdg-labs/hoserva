#!/usr/bin/env python3
"""Create GitHub issues from docs/roadmap.md, and write the numbers back.

The roadmap *seeds* issues; once an item has a number, the issue is the
source of truth (CLAUDE.md, "Issues are the plan"). This script only ever
writes in one direction — roadmap `issue:` fields — and never rewrites an
existing issue's body from roadmap text.

Epic membership, ordering dependencies and phases are GitHub's native fields
(sub-issue parent, blocked-by, milestone), never body prose.

    scripts/roadmap-sync.py --validate      # parse and validate only, no GitHub calls
    scripts/roadmap-sync.py                 # validate and show the plan (dry run)
    scripts/roadmap-sync.py M1.1 M1.2       # only those items (and their epics)
    scripts/roadmap-sync.py --apply         # create, link, write numbers back (prompts first)
    scripts/roadmap-sync.py --apply --yes   # no prompt

Exit codes: 0 ok, 1 validation failed, 2 a GitHub call failed.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
ROADMAP = REPO_ROOT / "docs" / "roadmap.md"
LABELS_FILE = REPO_ROOT / "scripts" / "bootstrap-labels.sh"
REPO = os.environ.get("GH_REPO", "mdg-labs/hoserva")

TYPE_LABELS = {"feat", "bug", "chore", "docs", "spike"}
EXTRA_LABELS = {"epic", "safety-critical", "needs-sudo", "needs-hardware", "blocked"}
STATUSES = {"todo", "doing", "blocked", "done"}
MIN_GH = (2, 100)

SUDO_PREAMBLE = (
    "> **Maintainer-run — this step needs root.** An agent prepares everything "
    "and prints the exact commands; it never runs `sudo`, `apt`, or edits "
    'anything under `/etc` itself (CLAUDE.md, "Root access is the maintainer\'s").'
)
HARDWARE_PREAMBLE = (
    "> **Needs real hardware.** Acceptance requires the L4 box or real disks, "
    "which only the maintainer touches (CLAUDE.md, \"Real disks are off-limits\"). "
    "An agent may prepare scripts and analysis, never run them against real devices."
)


# --------------------------------------------------------------------------
# Labels: scripts/bootstrap-labels.sh is the single source
# --------------------------------------------------------------------------


def load_labels() -> dict[str, tuple[str, str]]:
    labels: dict[str, tuple[str, str]] = {}
    for m in re.finditer(
        r'^\s*"([^|"]+)\|([0-9a-fA-F]{6})\|([^"]*)"', LABELS_FILE.read_text(), re.M
    ):
        labels[m.group(1)] = (m.group(2), m.group(3))
    missing = (TYPE_LABELS | EXTRA_LABELS) - labels.keys()
    if missing:
        raise SystemExit(f"{LABELS_FILE.name}: missing expected labels {sorted(missing)}")
    return labels


LABELS = load_labels()
AREA_LABELS = {n for n in LABELS if n.startswith("area:")}
STATUS_LABELS = {n for n in LABELS if n.startswith("status:")}
KNOWN_LABELS = TYPE_LABELS | AREA_LABELS | EXTRA_LABELS


# --------------------------------------------------------------------------
# gh plumbing
# --------------------------------------------------------------------------


class GhError(RuntimeError):
    pass


def gh(*args: str) -> str:
    proc = subprocess.run(["gh", *args], capture_output=True, text=True, cwd=REPO_ROOT)
    if proc.returncode != 0:
        raise GhError(f"gh {' '.join(args[:3])} …\n{proc.stderr.strip()}")
    return proc.stdout.strip()


def check_gh_version() -> None:
    m = re.search(r"gh version (\d+)\.(\d+)", gh("--version"))
    if not m or (int(m.group(1)), int(m.group(2))) < MIN_GH:
        raise SystemExit(
            f"gh >= {MIN_GH[0]}.{MIN_GH[1]} is required (sub-issue parent and "
            "blocked-by relationships)"
        )


def numbers_of(value: object) -> list[int]:
    """Issue numbers from a gh JSON relationship field, whatever its shape."""
    if isinstance(value, dict):
        value = value.get("nodes", [value] if "number" in value else [])
    if not isinstance(value, list):
        return []
    return [v["number"] for v in value if isinstance(v, dict) and "number" in v]


# --------------------------------------------------------------------------
# Parsing
# --------------------------------------------------------------------------


@dataclass
class Entity:
    kind: str  # "epic" | "item"
    ident: str
    title: str
    body: str
    block: str
    meta: dict[str, str]
    line: int
    issue: int | None = None
    items: list[str] = field(default_factory=list)

    def _list(self, key: str) -> list[str]:
        raw = self.meta.get(key, "[]").strip()
        return [x.strip() for x in raw.strip("[]").split(",") if x.strip()]

    @property
    def labels(self) -> list[str]:
        return self._list("labels")

    @property
    def depends(self) -> list[str]:
        return self._list("depends")

    def flag(self, key: str) -> bool:
        return self.meta.get(key, "false").strip().lower() == "true"

    @property
    def status(self) -> str:
        return self.meta.get("status", "todo").strip()


def parse(text: str) -> tuple[dict[str, str], list[Entity], list[Entity]]:
    """Return (phases, epics, items) in document order.

    Everything above the first `## M<n>` heading is format documentation; the
    only thing read from it is the ```phases block.
    """
    lines = text.splitlines()
    start = next((i for i, ln in enumerate(lines) if re.match(r"^## M\d+\b", ln)), None)
    if start is None:
        raise SystemExit("roadmap.md: no `## M<n>` epic heading found")

    header = "\n".join(lines[:start])
    pm = re.search(r"^```phases\n(.*?)^```", header, re.S | re.M)
    if not pm:
        raise SystemExit("roadmap.md: no ```phases block above the first epic")
    phases = dict(re.findall(r"^(P[\d.]+):[ \t]*(.+)$", pm.group(1), re.M))

    epics: list[Entity] = []
    items: list[Entity] = []
    current: Entity | None = None
    i = start
    while i < len(lines):
        line = lines[i]
        m_epic = re.match(r"^## (M\d+)\s+—\s+(.*)$", line)
        m_item = re.match(r"^### (.+)$", line)
        if not (m_epic or m_item):
            i += 1
            continue

        heading_line = i + 1
        kind = "epic" if m_epic else "item"
        fence = "epic" if m_epic else "meta"
        title = line[3:].strip() if m_epic else m_item.group(1).strip()
        i += 1
        while i < len(lines) and not lines[i].strip():
            i += 1
        if i >= len(lines) or lines[i].strip() != f"```{fence}":
            raise SystemExit(
                f"roadmap.md:{heading_line}: '{title}' is not followed by a ```{fence} block"
            )
        i += 1
        block_lines: list[str] = []
        while i < len(lines) and lines[i].strip() != "```":
            block_lines.append(lines[i])
            i += 1
        if i >= len(lines):
            raise SystemExit(f"roadmap.md:{heading_line}: unterminated ```{fence} block")
        i += 1
        block = "\n".join(block_lines)
        meta = dict(re.findall(r"^(\w+):[ \t]*(.*)$", block, re.M))

        prose: list[str] = []
        in_fence = False
        while i < len(lines):
            nxt = lines[i]
            if nxt.lstrip().startswith("```"):
                in_fence = not in_fence
            elif not in_fence and (re.match(r"^#{2,3} ", nxt) or nxt.strip() == "---"):
                break
            prose.append(nxt)
            i += 1

        ident = meta.get("id", "").strip()
        if not ident:
            raise SystemExit(f"roadmap.md:{heading_line}: '{title}' has no `id:`")
        issue_raw = meta.get("issue", "null").strip()
        ent = Entity(
            kind=kind,
            ident=ident,
            title=title,
            body="\n".join(prose).strip(),
            block=block,
            meta=meta,
            line=heading_line,
            issue=None if issue_raw in ("null", "") else int(issue_raw),
        )
        if kind == "epic":
            epics.append(ent)
            current = ent
        else:
            if current is None:
                raise SystemExit(f"{ident}: item appears before any epic")
            items.append(ent)
            current.items.append(ident)
    return phases, epics, items


# --------------------------------------------------------------------------
# Validation
# --------------------------------------------------------------------------


def validate(phases: dict[str, str], epics: list[Entity], items: list[Entity]) -> list[str]:
    problems: list[str] = []
    by_id = {e.ident: e for e in (*epics, *items)}
    epic_ids = {e.ident for e in epics}

    seen: set[str] = set()
    for e in (*epics, *items):
        if e.ident in seen:
            problems.append(f"{e.ident}: duplicate id")
        seen.add(e.ident)

    def check_labels(e: Entity) -> None:
        labs = e.labels
        for lab in labs:
            if lab in STATUS_LABELS:
                problems.append(f"{e.ident}: `{lab}` is machine-managed and never declared here")
            elif lab not in KNOWN_LABELS:
                problems.append(f"{e.ident}: label '{lab}' is not in scripts/bootstrap-labels.sh")
        types = [lab for lab in labs if lab in TYPE_LABELS]
        if len(types) != 1:
            problems.append(f"{e.ident}: needs exactly one type label, has {types or 'none'}")
        areas = [lab for lab in labs if lab in AREA_LABELS]
        if len(areas) > 1:
            problems.append(f"{e.ident}: at most one area label, has {areas}")
        if e.status not in STATUSES:
            problems.append(f"{e.ident}: status '{e.status}' is not one of {sorted(STATUSES)}")

    for ep in epics:
        if not re.fullmatch(r"M\d+", ep.ident):
            problems.append(f"{ep.ident}: epic ids are `M<n>`")
        if ep.meta.get("phase", "").strip() not in phases:
            problems.append(f"{ep.ident}: phase '{ep.meta.get('phase')}' is not in the ```phases block")
        if not ep.items:
            problems.append(f"{ep.ident}: epic has no items")
        check_labels(ep)

    for it in items:
        parent = it.meta.get("epic", "").strip()
        if parent not in epic_ids:
            problems.append(f"{it.ident}: epic '{parent}' does not exist")
        elif not re.fullmatch(rf"{re.escape(parent)}\.\d+", it.ident):
            problems.append(f"{it.ident}: item ids are `{parent}.<n>` inside epic {parent}")
        if "epic" in it.labels:
            problems.append(f"{it.ident}: `epic` belongs on epics only")
        for key, label in (("sudo", "needs-sudo"), ("hardware", "needs-hardware")):
            if it.flag(key) != (label in it.labels):
                problems.append(f"{it.ident}: `{key}: {it.meta.get(key, 'false')}` disagrees with label `{label}`")
        for dep in it.depends:
            if dep == it.ident:
                problems.append(f"{it.ident}: depends on itself")
            elif dep not in by_id or by_id[dep].kind != "item":
                problems.append(f"{it.ident}: depends on unknown item '{dep}'")
        if "**Acceptance criteria**" not in it.body:
            problems.append(f"{it.ident}: body has no **Acceptance criteria** — items are triaged, not stubs")
        check_labels(it)

    state: dict[str, int] = {}

    def visit(node: str, stack: list[str]) -> None:
        if state.get(node) == 1:
            problems.append("dependency cycle: " + " -> ".join(stack + [node]))
            return
        if state.get(node) == 2:
            return
        state[node] = 1
        for dep in by_id[node].depends:
            if dep in by_id:
                visit(dep, stack + [node])
        state[node] = 2

    for it in items:
        visit(it.ident, [])
    return problems


# --------------------------------------------------------------------------
# Rendering and write-back
# --------------------------------------------------------------------------


def render_body(e: Entity) -> str:
    chunks: list[str] = []
    if e.flag("sudo"):
        chunks.append(SUDO_PREAMBLE)
    if e.flag("hardware"):
        chunks.append(HARDWARE_PREAMBLE)
    chunks.append(e.body)
    chunks.append(f"---\nRoadmap: `{e.ident}` (docs/roadmap.md)")
    return "\n\n".join(c for c in chunks if c).strip() + "\n"


def write_issue_number(e: Entity, number: int) -> None:
    """Replace `issue: null` in one metadata block; re-reads the file each time,
    so a crash halfway leaves every created issue recorded."""
    text = ROADMAP.read_text()
    if e.block not in text:
        raise SystemExit(
            f"{e.ident}: its metadata block changed on disk — issue #{number} WAS "
            f"created; set `issue: {number}` by hand."
        )
    updated = e.block.replace("issue: null", f"issue: {number}", 1)
    ROADMAP.write_text(text.replace(e.block, updated, 1))
    e.block = updated
    e.issue = number


# --------------------------------------------------------------------------
# Main
# --------------------------------------------------------------------------


def create_issue(title: str, body: str, labels: list[str], milestone: str, parent: int | None) -> int:
    args = ["issue", "create", "--repo", REPO, "--title", title, "--body", body, "--milestone", milestone]
    for lab in labels:
        args += ["--label", lab]
    if parent:
        args += ["--parent", str(parent)]
    return int(gh(*args).rstrip("/").split("/")[-1])


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("ids", nargs="*", help="only sync these roadmap ids")
    ap.add_argument("--validate", action="store_true", help="parse and validate only")
    ap.add_argument("--apply", action="store_true", help="create issues and relationships")
    ap.add_argument("--yes", action="store_true", help="skip the confirmation")
    args = ap.parse_args()

    phases, epics, items = parse(ROADMAP.read_text())
    problems = validate(phases, epics, items)
    unknown = [i for i in args.ids if i not in {e.ident for e in (*epics, *items)}]
    problems += [f"unknown id on the command line: {i}" for i in unknown]
    if problems:
        print("Validation failed:\n")
        for p in problems:
            print(f"  ✗ {p}")
        return 1
    print(f"Parsed {len(phases)} phases, {len(epics)} epics, {len(items)} items — valid.")
    if args.validate:
        return 0

    by_id = {e.ident: e for e in (*epics, *items)}
    epic_of = {it.ident: by_id[it.meta["epic"].strip()] for it in items}
    milestone_of = lambda e: phases[(e if e.kind == "epic" else epic_of[e.ident]).meta["phase"].strip()]  # noqa: E731

    try:
        check_gh_version()
        gh("repo", "view", REPO, "--json", "nameWithOwner")

        wanted = set(args.ids) if args.ids else None
        sel_items = [
            it for it in items
            if (wanted is None or it.ident in wanted) and it.issue is None and it.status != "done"
        ]
        sel_epics = [
            ep for ep in epics
            if ep.issue is None
            and (wanted is None or ep.ident in wanted or any(epic_of[i.ident] is ep for i in sel_items))
        ]

        existing = json.loads(gh("issue", "list", "--repo", REPO, "--state", "all", "--limit", "1000", "--json", "number,title"))
        by_title = {e["title"]: e["number"] for e in existing}
        collisions = [(e.ident, e.title, by_title[e.title]) for e in (*sel_epics, *sel_items) if e.title in by_title]
        if collisions:
            print("Title collisions — stopping rather than creating duplicates:\n")
            for ident, title, num in collisions:
                print(f"  ✗ {ident}: #{num} is already titled {title!r}")
            return 1

        have_labels = {lab["name"] for lab in json.loads(gh("label", "list", "--repo", REPO, "--limit", "300", "--json", "name"))}
        need_labels = {"epic"} | STATUS_LABELS | {lab for e in (*sel_epics, *sel_items) for lab in e.labels}
        missing_labels = sorted(need_labels - have_labels)

        have_ms = set(gh("api", f"repos/{REPO}/milestones?state=all&per_page=100", "--paginate", "--jq", ".[].title").splitlines())
        missing_ms = sorted({milestone_of(e) for e in (*sel_epics, *sel_items)} - have_ms)

        total = len(sel_epics) + len(sel_items)
        if not args.apply:
            print(f"Target repo: {REPO}\nDRY RUN — nothing will be created. Re-run with --apply.\n")
            if missing_labels:
                print(f"Labels to create: {', '.join(missing_labels)}  (or run scripts/bootstrap-labels.sh)")
            if missing_ms:
                print(f"Milestones to create: {', '.join(missing_ms)}")
            print(f"\nIssues to create ({total}):")
            for ep in epics:
                ep_items = [i for i in sel_items if epic_of[i.ident] is ep]
                if ep not in sel_epics and not ep_items:
                    continue
                num = f"#{ep.issue}" if ep.issue else "new"
                print(f"  [epic {num}] {ep.title}   ({milestone_of(ep)})")
                for it in ep_items:
                    flags = "".join(f" [{f}]" for f in ("safety-critical", "needs-sudo", "needs-hardware") if f in it.labels)
                    deps = f"  ← {', '.join(it.depends)}" if it.depends else ""
                    print(f"      {it.ident:<7} {it.title}{flags}{deps}")
            return 0

        if total > 10 and not args.yes:
            print(f"About to create {total} issues in {REPO} — visible, hard-to-undo work.")
            if input("Type 'yes' to continue: ").strip().lower() != "yes":
                print("Aborted — nothing created.")
                return 0

        for name in missing_labels:
            colour, desc = LABELS.get(name, ("ededed", ""))
            gh("label", "create", name, "--repo", REPO, "--color", colour, "--description", desc)
        for title in missing_ms:
            gh("api", "--method", "POST", f"repos/{REPO}/milestones", "-f", f"title={title}")

        for ep in epics:
            if ep in sel_epics:
                num = create_issue(ep.title, render_body(ep), sorted(set(ep.labels) | {"epic"}), milestone_of(ep), None)
                write_issue_number(ep, num)
                print(f"  epic #{num}  {ep.title}")
            for it in [i for i in sel_items if epic_of[i.ident] is ep]:
                if ep.issue is None:
                    raise SystemExit(f"{it.ident}: epic {ep.ident} has no issue number")
                num = create_issue(it.title, render_body(it), sorted(it.labels), milestone_of(it), ep.issue)
                write_issue_number(it, num)
                print(f"    sub #{num}  {it.ident}  {it.title}")

        # Relationship pass over every filed item, not only this run's, so a
        # re-run heals a link that failed or a dependency filed later.
        fixed = 0
        pending: list[str] = []
        for it in items:
            if it.issue is None:
                continue
            ep = epic_of[it.ident]
            cur = json.loads(gh("issue", "view", str(it.issue), "--repo", REPO, "--json", "parent,blockedBy,milestone"))
            edit: list[str] = []
            parent = numbers_of(cur.get("parent"))
            if ep.issue and parent != [ep.issue]:
                edit += ["--parent", str(ep.issue)]
            if (cur.get("milestone") or {}).get("title") != milestone_of(it):
                edit += ["--milestone", milestone_of(it)]
            have = set(numbers_of(cur.get("blockedBy")))
            want = []
            for dep in it.depends:
                if by_id[dep].issue is None:
                    pending.append(f"{it.ident} ← {dep}")
                elif by_id[dep].issue not in have:
                    want.append(str(by_id[dep].issue))
            if want:
                edit += ["--add-blocked-by", ",".join(want)]
            if edit:
                gh("issue", "edit", str(it.issue), "--repo", REPO, *edit)
                fixed += 1
    except GhError as exc:
        print(f"\nStopped: {exc}", file=sys.stderr)
        print("Created issue numbers were written back as they went — re-run to continue.", file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        print("\nInterrupted — re-run to continue.", file=sys.stderr)
        return 2

    print(f"\nDone: {total} issues created, {fixed} issues' relationships updated.")
    if pending:
        print(f"Blocked-by not linked yet (dependency not filed): {', '.join(pending)}")
    print("Roadmap `issue:` fields updated. Nothing was committed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
