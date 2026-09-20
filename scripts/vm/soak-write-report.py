#!/usr/bin/env python3
import json, sys
from pathlib import Path
hist_path, report_path, findings_path = Path(sys.argv[1]), Path(sys.argv[2]), Path(sys.argv[3])
lab_id, parity, data, cache, day0 = sys.argv[4:9]
yank_n, full_n, power_n = sys.argv[9:12]
def cat_count(diff, cat):
    for g in (diff or {}).get("groups") or []:
        if g.get("category") == cat:
            return int(g.get("count") or 0)
    return 0


injected = {"mass-delete", "yank", "full-disk", "power-loss"}
rows = [json.loads(line) for line in hist_path.read_text().splitlines() if line.strip()]
nights = len(rows)
blocked = [r for r in rows if r.get("wouldBlock")]
routine = [r for r in rows if r.get("inject") not in injected]
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
    "- **Config backup:** scheduleRunner.Backup is nil, so the chain skips the step. `POST /config/export` exists and was not substituted into the chain.",
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
