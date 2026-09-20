# L3 soak report (issue #44)

Lab id: `210-a1`
Nights: 30
Guest calendar start: 2026-09-21 (time-compressed; chains ran back to back, Q30)
Array: 1×2G parity, 5×2G data, 1×512M cache; minfreespace 32M (soak-sized so a full-disk injection can actually fill a disk)

## Chain behaviour

Q30 order is mover → diff + guard → (touch, Q17) → sync → config backup, with weekly scrub after sync on Sunday.
- **Mover:** TypeMover is not registered in hoservad; MaintenanceChain skips it (`ErrJobTypeNotRegistered`). Phase 2 product work. No second placement algorithm was invented.
- **Config backup:** `scheduleRunner.Backup` is the `backup.Service` from `newBackupService` (boot destination under the state dir), so the Q30 `config_backup` step runs in the chain. It was not substituted with a lone `POST /config/export`.
- **Sync / scrub / guard:** driven by the real nightly chain (ClaimDueChain + MaintenanceChain.Run) after advancing the guest clock past 02:00 each simulated day.

## Injected failures

| Night | Failure | Recovery |
|---|---|---|
| 12 | Yanked data disk (live+config detach of disk1) | Reattached the same qcow2; `array start`; SHA-256 of data-disk files vs pre-yank manifest |
| 18 | Filled /mnt/disk1 with soak-fill.bin | Removed the filler; SHA-256 of other data-disk files vs pre-fill manifest |
| 24 | `virsh destroy` while a sync job was running | Domain started; hoservad came back; SHA-256 vs pre-loss manifest; sync re-issued with confirm |

## Blocked syncs

30 nights recorded; 3 blocked.

- Night 8 (2026-09-28 Monday, inject=mass-delete): removedCount=600 removed+updated=35.608307% triggers=removed-count,removed-updated-percent. blocked: removed=600 pct=35.608307 triggers=removed-count,removed-updated-percent summary=600 removed files exceed the threshold; 35.6% removed+updated exceeds the threshold — expected: mass-delete churn exceeded Q16 (500 / 10%). Human review then force-sync so later nights are not stuck on the same diff.
- Night 12 (2026-10-02 Friday, inject=yank): removedCount=9 removed+updated=0.7992895% triggers=zero-files. blocked: removed=9 pct=0.7992895 triggers=zero-files summary=/mnt/disk1 dropped to zero files (had 1) — expected while disk1 was yanked (zero-files and/or mass removals). Disk reattached; checksums compared.
- Night 22 (2026-10-12 Monday, inject=mass-delete): removedCount=600 removed+updated=48.154095% triggers=removed-count,removed-updated-percent. blocked: removed=600 pct=48.154095 triggers=removed-count,removed-updated-percent summary=600 removed files exceed the threshold; 48.2% removed+updated exceeds the threshold — expected: mass-delete churn exceeded Q16 (500 / 10%). Human review then force-sync so later nights are not stuck on the same diff.

## Q16 — guard thresholds

**Default kept: 500 removed files / 10% removed+updated.**

Routine nights (n=25, no injected failure) max snapraid-removed=16, max updated=15. Mass-delete nights removed=[600, 600] (removed+updated [35.608307, 48.154095]). Confirmed. The soak's routine nightly diffs stay well under 500 / 10%, and the seeded mass-delete nights trip both Q16 triggers as designed. Raising the thresholds would let a ransomware-scale deletion through; lowering them would block ordinary churn this run never produced (the API omits guard.removedCount unless the guard actually blocks).

Evidence: `scripts/vm/soak-history.jsonl` (one JSON object per night, including the pre-chain `POST /parity/diff` result).

## Residual risk (D20)

Virtio-blk qcow2 is not a real SATA/SAS disk. Firmware spindown, SMART standby honouring, and enclosure identity quirks are not proven here (doc 06 §6). Crash consistency of SnapRAID under `virsh destroy` is the hypervisor's sudden-power-loss proxy, not a pulled power cord on real electronics.

## Findings

none
