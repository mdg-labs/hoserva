#!/usr/bin/env python3
import json, sys
night = int(sys.argv[1])
day, weekday, inject = sys.argv[2], sys.argv[3], sys.argv[4]
churn = json.load(open(sys.argv[5]))
diff = json.load(open(sys.argv[6]))
try:
    jobs = json.load(open(sys.argv[7]))
except Exception:
    jobs = None
hist_path, findings_path = sys.argv[8], sys.argv[9]
def cat_count(diff, cat):
    for g in (diff or {}).get("groups") or []:
        if g.get("category") == cat:
            return int(g.get("count") or 0)
    return 0

guard = diff.get("guard") or {}
would_block = bool(guard.get("wouldBlock"))
removed = guard.get("removedCount")
if removed is None:
    removed = cat_count(diff, "removed")
pct = guard.get("removedUpdatedPercent")
if pct is None:
    pct = 0
triggers = ",".join(guard.get("triggers") or [])
summary = guard.get("summary") or ""
Q16_TRIGGERS = {"removed-count", "removed-updated-percent"}
have_triggers = {t for t in (guard.get("triggers") or []) if t}
notes = ""
if inject == "mass-delete":
    missing = Q16_TRIGGERS - have_triggers
    if not would_block or missing:
        notes = (
            f"mass-delete did not trip both Q16 triggers: "
            f"wouldBlock={would_block} triggers={triggers} summary={summary}"
        )
        with open(findings_path, "a") as fh:
            fh.write(f"night {night}: {notes}\n")
    else:
        notes = (
            f"blocked: removed={removed} pct={pct} triggers={triggers} summary={summary}"
            " — expected: mass-delete churn exceeded Q16 (500 / 10%). "
            "Human review then force-sync so later nights are not stuck on the same diff."
        )
elif would_block:
    notes = f"blocked: removed={removed} pct={pct} triggers={triggers} summary={summary}"
    if inject == "yank":
        notes += " — expected while disk1 was yanked (zero-files and/or mass removals). Disk reattached; checksums compared."
    else:
        notes += " — unexpected block on a non-mass-delete night"
        with open(findings_path, "a") as fh:
            fh.write(f"night {night}: unexpected guard block ({notes})\n")
rec = {
    "night": night,
    "date": day,
    "weekday": weekday,
    "inject": inject,
    "churn": churn,
    "diff": diff,
    "wouldBlock": would_block,
    "removedCount": removed,
    "removedUpdatedPercent": pct,
    "triggers": triggers,
    "notes": notes,
    "jobs": jobs,
}
with open(hist_path, "a") as fh:
    json.dump(rec, fh, default=str)
    fh.write("\n")
print(json.dumps({"night": night, "wouldBlock": would_block}))
sys.exit(10 if would_block else 0)
