#!/usr/bin/env python3
import json, sys
from pathlib import Path

INJECTED = {"mass-delete", "yank", "full-disk", "power-loss"}
Q16_REMOVED_MAX = 500
Q16_REMOVED_UPDATED_PERCENT = 10.0
Q16_MASS_TRIGGERS = frozenset({"removed-count", "removed-updated-percent"})


def cat_count(diff, cat):
    for g in (diff or {}).get("groups") or []:
        if g.get("category") == cat:
            return int(g.get("count") or 0)
    return 0


def trigger_set(row):
    raw = row.get("triggers") or ""
    if isinstance(raw, (list, tuple, set)):
        return {str(t) for t in raw if t}
    return {t for t in str(raw).split(",") if t}


def q16_failures(rows):
    failures = []
    routine = [r for r in rows if r.get("inject") not in INJECTED]
    mass = [r for r in rows if r.get("inject") == "mass-delete"]
    if len(mass) < 2:
        failures.append(f"want at least 2 mass-delete nights, got {len(mass)}")
    for r in routine:
        night = r.get("night")
        removed = cat_count(r.get("diff"), "removed")
        pct = float(r.get("removedUpdatedPercent") or 0)
        if r.get("wouldBlock"):
            failures.append(f"night {night}: routine churn blocked the guard")
        if removed >= Q16_REMOVED_MAX:
            failures.append(f"night {night}: routine removed={removed} >= {Q16_REMOVED_MAX}")
        if pct >= Q16_REMOVED_UPDATED_PERCENT:
            failures.append(
                f"night {night}: routine removed+updated={pct}% >= {Q16_REMOVED_UPDATED_PERCENT}%"
            )
    for r in mass:
        night = r.get("night")
        have = trigger_set(r)
        missing = Q16_MASS_TRIGGERS - have
        if not r.get("wouldBlock"):
            failures.append(f"night {night}: mass-delete did not block")
        if missing:
            failures.append(
                f"night {night}: mass-delete missing triggers {sorted(missing)} (got {sorted(have)})"
            )
    return failures


def main() -> None:
    hist_path, report_path, findings_path = Path(sys.argv[1]), Path(sys.argv[2]), Path(sys.argv[3])
    lab_id, parity, data, cache, day0 = sys.argv[4:9]
    yank_n, full_n, power_n = sys.argv[9:12]
    rows = [json.loads(line) for line in hist_path.read_text().splitlines() if line.strip()]
    nights = len(rows)
    blocked = [r for r in rows if r.get("wouldBlock")]
    routine = [r for r in rows if r.get("inject") not in INJECTED]
    routine_removed = [cat_count(r.get("diff"), "removed") for r in routine]
    routine_updated = [cat_count(r.get("diff"), "updated") for r in routine]
    mass = [r for r in rows if r.get("inject") == "mass-delete"]
    max_routine_removed = max(routine_removed) if routine_removed else 0
    max_routine_updated = max(routine_updated) if routine_updated else 0
    mass_removed = [int((r.get("removedCount") if r.get("removedCount") is not None else cat_count(r.get("diff"), "removed")) or 0) for r in mass]
    mass_pct = [float(r.get("removedUpdatedPercent") or 0) for r in mass]
    rationale = (
        f"Routine nights (n={len(routine)}, no injected failure) "
        f"max snapraid-removed={max_routine_removed}, max updated={max_routine_updated}. "
        f"Mass-delete nights removed={mass_removed} (removed+updated {mass_pct}). "
    )
    failures = q16_failures(rows)
    if failures:
        verdict = (
            "Failed. The soak history does not meet Q16: "
            + "; ".join(failures)
            + "."
        )
    else:
        verdict = (
            "Confirmed. The soak's routine nightly diffs stay well under 500 / 10%, "
            "and the seeded mass-delete nights trip both Q16 triggers as designed. Raising the "
            "thresholds would let a ransomware-scale deletion through; lowering them "
            "would block ordinary churn this run never produced (the API omits "
            "guard.removedCount unless the guard actually blocks)."
        )
    lines = [
        "# L3 soak report (issue #44)",
        "",
        f"Lab id: `{lab_id}`",
        f"Nights: {nights}",
        f"Guest calendar start: {day0} (time-compressed; chains ran back to back, Q30)",
        f"Array: 1×{parity} parity, 5×{data} data, 1×{cache} cache; minfreespace 32M (soak-sized so a full-disk injection can actually fill a disk)",
        "",
        "## Chain behaviour",
        "",
        "Q30 order is mover → diff + guard → (touch, Q17) → sync → config backup, with weekly scrub after sync on Sunday.",
        "- **Mover:** TypeMover is not registered in hoservad; MaintenanceChain skips it (`ErrJobTypeNotRegistered`). Phase 2 product work. No second placement algorithm was invented.",
        "- **Config backup:** `scheduleRunner.Backup` is the `backup.Service` from `newBackupService` (boot destination under the state dir), so the Q30 `config_backup` step runs in the chain. It was not substituted with a lone `POST /config/export`.",
        "- **Sync / scrub / guard:** driven by the real nightly chain (ClaimDueChain + MaintenanceChain.Run) after advancing the guest clock past 02:00 each simulated day.",
        "",
        "## Injected failures",
        "",
        "| Night | Failure | Recovery |",
        "|---|---|---|",
        f"| {yank_n} | Yanked data disk (live+config detach of disk1) | Reattached the same qcow2; `array start`; SHA-256 of data-disk files vs pre-yank manifest |",
        f"| {full_n} | Filled /mnt/disk1 with soak-fill.bin | Removed the filler; SHA-256 of other data-disk files vs pre-fill manifest |",
        f"| {power_n} | `virsh destroy` while a sync job was running | Domain started; hoservad came back; SHA-256 vs pre-loss manifest; sync re-issued with confirm |",
        "",
        "## Blocked syncs",
        "",
        f"{nights} nights recorded; {len(blocked)} blocked.",
        "",
    ]
    for r in blocked:
        lines.append(
            f"- Night {r['night']} ({r['date']} {r['weekday']}, inject={r.get('inject') or 'none'}): "
            f"removedCount={r.get('removedCount')} removed+updated={r.get('removedUpdatedPercent')}% "
            f"triggers={r.get('triggers')}. {r.get('notes')}"
        )
    lines += [
        "",
        "## Q16 — guard thresholds",
        "",
        "**Default kept: 500 removed files / 10% removed+updated.**",
        "",
        rationale + verdict,
        "",
        "Evidence: `scripts/vm/soak-history.jsonl` (one JSON object per night, including the pre-chain `POST /parity/diff` result).",
        "",
        "## Residual risk (D20)",
        "",
        "Virtio-blk qcow2 is not a real SATA/SAS disk. Firmware spindown, SMART standby honouring, and enclosure identity quirks are not proven here (doc 06 §6). Crash consistency of SnapRAID under `virsh destroy` is the hypervisor's sudden-power-loss proxy, not a pulled power cord on real electronics.",
        "",
        "## Findings",
        "",
    ]
    raw = findings_path.read_text().splitlines() if findings_path.exists() else []
    raw = [f for f in raw if f.strip()]
    if not raw:
        lines.append("none")
    else:
        lines.extend(f"- {f}" for f in raw)
    lines.append("")
    report_path.write_text("\n".join(lines))
    print(report_path.read_text())
    if failures:
        print("soak-write-report: Q16 criteria failed:", "; ".join(failures), file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
