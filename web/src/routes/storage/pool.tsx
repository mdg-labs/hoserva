import { useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { ConfirmDialog } from "@/components/patterns/confirm";
import { FormOverlay } from "@/components/patterns/form-overlay";
import { InlineNote } from "@/components/patterns/inline-note";
import { LoadingBlock } from "@/components/patterns/loading";
import { MetricTile } from "@/components/patterns/metric-tile";
import { StatusBadge } from "@/components/patterns/status-badge";
import { SelectFilter } from "@/components/patterns/table-filters";
import { TypedConfirm } from "@/components/patterns/typed-confirm";
import { Button } from "@/components/ui/button";
import { Field, FieldLabel } from "@/components/ui/field";
import { Switch } from "@/components/ui/switch";
import { useSystemData } from "@/hooks/use-system-status";
import {
  postArrayAdd,
  postArrayAddPlan,
  postArrayReplace,
  postArrayReplacePlan,
  postArrayStart,
  postArrayStop,
  postArrayUpgrade,
  postArrayUpgradePlan,
  postDiskEvacuation,
  postDiskEvacuationPlan,
  postPoolRebalance,
  postPoolRebalancePlan,
} from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import type { components } from "@/lib/api/client";
import { formatBytes } from "@/routes/storage-setup/config-preview";
import type { TFunction } from "i18next";

type AddDiskPlan = components["schemas"]["AddDiskPlan"];
type ReplaceDiskPlan = components["schemas"]["ReplaceDiskPlan"];
type DiskUpgradePlan = components["schemas"]["DiskUpgradePlan"];
type RebalancePlan = components["schemas"]["RebalancePlan"];
type EvacuationPlan = components["schemas"]["EvacuationPlan"];
type ArrayDiskFilesystem = components["schemas"]["ArrayDiskFilesystem"];
type PoolDiskEntry = components["schemas"]["PoolDiskEntry"];

const FILESYSTEM_OPTIONS: { value: ArrayDiskFilesystem; label: string }[] = [
  { value: "xfs", label: "XFS" },
  { value: "ext4", label: "ext4" },
  { value: "btrfs", label: "btrfs" },
];

// diskIdentitySummary renders a plan's target/replacement identity (doc 02
// §4, finding 3 of #288's fix round: model, WWN or serial, size, its
// existing filesystem) as one line the operator can recognise the disk by
// before typing a destructive confirmation — never just its /dev/sdX path,
// which a reboot or hotplug can reassign to a different disk entirely.
// Returns null when the plan carries no identity at all (a loop device in
// the lab, doc 06 §3, has none to show).
function diskIdentitySummary(
  t: TFunction,
  plan: Pick<AddDiskPlan, "model" | "wwn" | "serial" | "sizeBytes" | "currentFilesystem">,
): string | null {
  const parts: string[] = [];
  if (plan.model) parts.push(plan.model);
  if (plan.wwn) parts.push(t("pool.identity.wwn", { wwn: plan.wwn }));
  else if (plan.serial) parts.push(t("pool.identity.serial", { serial: plan.serial }));
  if (plan.sizeBytes) parts.push(formatBytes(plan.sizeBytes));
  if (plan.currentFilesystem) parts.push(t("pool.identity.currentFilesystem", { filesystem: plan.currentFilesystem }));
  return parts.length > 0 ? parts.join(" · ") : null;
}

// slotLabel names a data-disk pool entry for the "Disk to replace" select:
// its mountpoint plus its device, or a plain-language "missing" marker
// when the stored slot has no device present (#326's own reconciliation —
// exactly the slot doc 02 §4 "Replacing a failed disk" exists for).
function slotLabel(t: TFunction, entry: PoolDiskEntry): string {
  if (entry.state === "missing" || !entry.device) {
    return t("pool.replace.slotMissing", { mountpoint: entry.mountPoint });
  }
  return `${entry.mountPoint} (${entry.device})`;
}

// upgradeSlotLabel names a data-or-parity pool entry for the "Disk to
// upgrade" select (doc 02 §4) — slotLabel's shape, plus its role, since
// the list mixes both.
function upgradeSlotLabel(entry: PoolDiskEntry): string {
  return `${entry.mountPoint} (${entry.role}, ${entry.device})`;
}

// rebalancePlanItems renders a RebalancePlan or EvacuationPlan's own
// moves and warnings (doc 09 §3-4) as the TypedConfirm item list: a plain
// count/size summary first, since the exact per-file list can run into
// the hundreds, then every path-preserving warning the plan carries.
function rebalancePlanItems(t: TFunction, plan: Pick<RebalancePlan, "moves" | "warnings">): string[] {
  const totalBytes = plan.moves.reduce((sum, move) => sum + (move.sizeBytes ?? 0), 0);
  const items = [t("pool.planItems.summaryItem", { count: plan.moves.length, size: formatBytes(totalBytes) })];
  for (const warning of plan.warnings) {
    items.push(t("pool.planItems.warningItem", { share: warning.share, reason: warning.reason }));
  }
  return items;
}

export function PoolOverviewPage(): React.ReactElement {
  const { t } = useTranslation();
  const { status, pool, loading, error, refresh } = useSystemData();
  const [stopOpen, setStopOpen] = useState(false);
  const [startOpen, setStartOpen] = useState(false);
  const [pending, setPending] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const maintenance = Boolean(status?.maintenanceMode);

  const [addOpen, setAddOpen] = useState(false);
  const [addDevice, setAddDevice] = useState("");
  const [addFilesystem, setAddFilesystem] = useState<ArrayDiskFilesystem>("xfs");
  const [addAdopt, setAddAdopt] = useState(false);
  const [addPlan, setAddPlan] = useState<AddDiskPlan | null>(null);
  const [addConfirmValue, setAddConfirmValue] = useState("");
  const [addError, setAddError] = useState<string | null>(null);
  const [addPending, setAddPending] = useState(false);
  // addSelectionGen bumps on every field change the add dialog offers, so
  // previewAddDisk can tell — after its own await returns — whether the
  // selection it was asked to preview is still the one on screen. A ref,
  // not state: it must read as current inside a closure created before the
  // selection changed, which a state variable captured at call time cannot
  // (finding 4 of #288's fix round).
  const addSelectionGen = useRef(0);

  const [replaceOpen, setReplaceOpen] = useState(false);
  const [replaceMountpoint, setReplaceMountpoint] = useState("");
  const [replaceDevice, setReplaceDevice] = useState("");
  const [replaceFilesystem, setReplaceFilesystem] = useState<ArrayDiskFilesystem>("xfs");
  const [replaceAdopt, setReplaceAdopt] = useState(false);
  const [replacePlan, setReplacePlan] = useState<ReplaceDiskPlan | null>(null);
  const [replaceConfirmValue, setReplaceConfirmValue] = useState("");
  const [replaceError, setReplaceError] = useState<string | null>(null);
  const [replacePending, setReplacePending] = useState(false);
  const replaceSelectionGen = useRef(0);

  const [upgradeOpen, setUpgradeOpen] = useState(false);
  const [upgradeMountpoint, setUpgradeMountpoint] = useState("");
  const [upgradeDevice, setUpgradeDevice] = useState("");
  const [upgradeFilesystem, setUpgradeFilesystem] = useState<ArrayDiskFilesystem>("xfs");
  const [upgradePlan, setUpgradePlan] = useState<DiskUpgradePlan | null>(null);
  const [upgradeConfirmValue, setUpgradeConfirmValue] = useState("");
  const [upgradeError, setUpgradeError] = useState<string | null>(null);
  const [upgradePending, setUpgradePending] = useState(false);
  const upgradeSelectionGen = useRef(0);

  const [rebalanceOpen, setRebalanceOpen] = useState(false);
  const [rebalancePlan, setRebalancePlan] = useState<RebalancePlan | null>(null);
  const [rebalanceConfirmValue, setRebalanceConfirmValue] = useState("");
  const [rebalanceError, setRebalanceError] = useState<string | null>(null);
  const [rebalancePending, setRebalancePending] = useState(false);

  const [removeOpen, setRemoveOpen] = useState(false);
  const [removeMountpoint, setRemoveMountpoint] = useState("");
  const [removePlan, setRemovePlan] = useState<EvacuationPlan | null>(null);
  const [removeConfirmValue, setRemoveConfirmValue] = useState("");
  const [removeError, setRemoveError] = useState<string | null>(null);
  const [removePending, setRemovePending] = useState(false);
  const removeSelectionGen = useRef(0);

  const addPlanMutation = useApiMutation({ mutationFn: postArrayAddPlan });
  const addMutation = useApiMutation({ mutationFn: postArrayAdd });
  const replacePlanMutation = useApiMutation({ mutationFn: postArrayReplacePlan });
  const replaceMutation = useApiMutation({ mutationFn: postArrayReplace });
  const upgradePlanMutation = useApiMutation({ mutationFn: postArrayUpgradePlan });
  const upgradeMutation = useApiMutation({ mutationFn: postArrayUpgrade });
  const rebalancePlanMutation = useApiMutation({ mutationFn: postPoolRebalancePlan });
  const rebalanceMutation = useApiMutation({ mutationFn: postPoolRebalance });
  const removePlanMutation = useApiMutation({ mutationFn: postDiskEvacuationPlan });
  const removeMutation = useApiMutation({ mutationFn: postDiskEvacuation });
  const stopMutation = useApiMutation({ mutationFn: postArrayStop });
  const startMutation = useApiMutation({ mutationFn: postArrayStart });

  function resetAddPlan(): void {
    setAddPlan(null);
    setAddConfirmValue("");
    setAddError(null);
    // A selection change makes any in-flight preview request irrelevant
    // (addSelectionGen already marks it stale) — clearing pending here too
    // so Preview is immediately available again, rather than staying
    // disabled until that now-discarded response arrives.
    setAddPending(false);
  }

  function resetAddDialog(): void {
    resetAddPlan();
    setAddDevice("");
    setAddFilesystem("xfs");
    setAddAdopt(false);
  }

  async function previewAddDisk(): Promise<void> {
    if (!addDevice) return;
    addSelectionGen.current += 1;
    const gen = addSelectionGen.current;
    const device = addDevice;
    const filesystem = addFilesystem;
    const adopt = addAdopt;
    setAddPending(true);
    setAddError(null);
    const result = await addPlanMutation.mutate({ device, filesystem, adopt });
    if (addSelectionGen.current !== gen) return;
    if (!result.ok) {
      setAddError(result.error);
      setAddPending(false);
      return;
    }
    setAddPlan(result.data ?? null);
    setAddConfirmValue("");
    if (addSelectionGen.current === gen) setAddPending(false);
  }

  async function submitAddDisk(): Promise<void> {
    if (!addPlan) return;
    setAddPending(true);
    setAddError(null);
    const result = await addMutation.mutate({
      device: addPlan.device,
      filesystem: addPlan.filesystem,
      adopt: addPlan.adopt,
      confirmation: addConfirmValue,
    });
    if (!result.ok) {
      setAddError(result.error);
      setAddPending(false);
      return;
    }
    setAddOpen(false);
    resetAddDialog();
    await refresh();
    setAddPending(false);
  }

  function resetReplacePlan(): void {
    setReplacePlan(null);
    setReplaceConfirmValue("");
    setReplaceError(null);
    // See resetAddPlan: an in-flight preview for the old selection is
    // already stale (replaceSelectionGen), so Preview should not stay
    // disabled waiting for it.
    setReplacePending(false);
  }

  function resetReplaceDialog(): void {
    resetReplacePlan();
    setReplaceMountpoint("");
    setReplaceDevice("");
    setReplaceFilesystem("xfs");
    setReplaceAdopt(false);
  }

  async function previewReplaceDisk(): Promise<void> {
    if (!replaceMountpoint || !replaceDevice) return;
    replaceSelectionGen.current += 1;
    const gen = replaceSelectionGen.current;
    const mountpoint = replaceMountpoint;
    const device = replaceDevice;
    const filesystem = replaceFilesystem;
    const adopt = replaceAdopt;
    setReplacePending(true);
    setReplaceError(null);
    const result = await replacePlanMutation.mutate({ mountpoint, device, filesystem, adopt });
    if (replaceSelectionGen.current !== gen) return;
    if (!result.ok) {
      setReplaceError(result.error);
      setReplacePending(false);
      return;
    }
    setReplacePlan(result.data ?? null);
    setReplaceConfirmValue("");
    if (replaceSelectionGen.current === gen) setReplacePending(false);
  }

  async function submitReplaceDisk(): Promise<void> {
    if (!replacePlan) return;
    setReplacePending(true);
    setReplaceError(null);
    const result = await replaceMutation.mutate({
      mountpoint: replacePlan.mountpoint,
      device: replacePlan.replacementDevice,
      filesystem: replacePlan.filesystem,
      adopt: replacePlan.adopt,
      confirmation: replaceConfirmValue,
    });
    if (!result.ok) {
      setReplaceError(result.error);
      setReplacePending(false);
      return;
    }
    setReplaceOpen(false);
    resetReplaceDialog();
    await refresh();
    setReplacePending(false);
  }

  function resetUpgradePlan(): void {
    setUpgradePlan(null);
    setUpgradeConfirmValue("");
    setUpgradeError(null);
    // See resetAddPlan: an in-flight preview for the old selection is
    // already stale (upgradeSelectionGen), so Preview should not stay
    // disabled waiting for it.
    setUpgradePending(false);
  }

  function resetUpgradeDialog(): void {
    resetUpgradePlan();
    setUpgradeMountpoint("");
    setUpgradeDevice("");
    setUpgradeFilesystem("xfs");
  }

  async function previewUpgradeDisk(): Promise<void> {
    if (!upgradeMountpoint || !upgradeDevice) return;
    upgradeSelectionGen.current += 1;
    const gen = upgradeSelectionGen.current;
    const mountpoint = upgradeMountpoint;
    const device = upgradeDevice;
    const filesystem = upgradeFilesystem;
    setUpgradePending(true);
    setUpgradeError(null);
    const result = await upgradePlanMutation.mutate({ mountpoint, device, filesystem });
    if (upgradeSelectionGen.current !== gen) return;
    if (!result.ok) {
      setUpgradeError(result.error);
      setUpgradePending(false);
      return;
    }
    setUpgradePlan(result.data ?? null);
    setUpgradeConfirmValue("");
    if (upgradeSelectionGen.current === gen) setUpgradePending(false);
  }

  async function submitUpgradeDisk(): Promise<void> {
    if (!upgradePlan) return;
    setUpgradePending(true);
    setUpgradeError(null);
    const result = await upgradeMutation.mutate({
      mountpoint: upgradePlan.mountpoint,
      device: upgradePlan.replacementDevice,
      filesystem: upgradePlan.filesystem,
      newMountpoint: upgradePlan.newMountpoint,
      confirmation: upgradeConfirmValue,
    });
    if (!result.ok) {
      setUpgradeError(result.error);
      setUpgradePending(false);
      return;
    }
    setUpgradeOpen(false);
    resetUpgradeDialog();
    await refresh();
    setUpgradePending(false);
  }

  function resetRebalanceDialog(): void {
    setRebalancePlan(null);
    setRebalanceConfirmValue("");
    setRebalanceError(null);
    setRebalancePending(false);
  }

  // previewRebalance runs as soon as the Rebalance dialog opens — unlike
  // Add/Replace/Upgrade, there is no selection to make first (doc 09 §3
  // computes the plan from the pool's own current state alone).
  async function previewRebalance(): Promise<void> {
    setRebalancePending(true);
    setRebalanceError(null);
    const result = await rebalancePlanMutation.mutate(undefined);
    if (!result.ok) {
      setRebalanceError(result.error);
      setRebalancePending(false);
      return;
    }
    setRebalancePlan(result.data ?? null);
    setRebalanceConfirmValue("");
    setRebalancePending(false);
  }

  async function submitRebalance(): Promise<void> {
    if (!rebalancePlan) return;
    setRebalancePending(true);
    setRebalanceError(null);
    const result = await rebalanceMutation.mutate({ confirmation: rebalanceConfirmValue });
    if (!result.ok) {
      setRebalanceError(result.error);
      setRebalancePending(false);
      return;
    }
    setRebalanceOpen(false);
    resetRebalanceDialog();
    await refresh();
    setRebalancePending(false);
  }

  function resetRemovePlan(): void {
    setRemovePlan(null);
    setRemoveConfirmValue("");
    setRemoveError(null);
    // See resetAddPlan: an in-flight preview for the old selection is
    // already stale (removeSelectionGen), so Preview should not stay
    // disabled waiting for it.
    setRemovePending(false);
  }

  function resetRemoveDialog(): void {
    resetRemovePlan();
    setRemoveMountpoint("");
  }

  async function previewRemoveDisk(): Promise<void> {
    if (!removeMountpoint) return;
    removeSelectionGen.current += 1;
    const gen = removeSelectionGen.current;
    const mountpoint = removeMountpoint;
    setRemovePending(true);
    setRemoveError(null);
    const result = await removePlanMutation.mutate({ mountpoint });
    if (removeSelectionGen.current !== gen) return;
    if (!result.ok) {
      setRemoveError(result.error);
      setRemovePending(false);
      return;
    }
    setRemovePlan(result.data ?? null);
    setRemoveConfirmValue("");
    if (removeSelectionGen.current === gen) setRemovePending(false);
  }

  async function submitRemoveDisk(): Promise<void> {
    if (!removePlan) return;
    setRemovePending(true);
    setRemoveError(null);
    const result = await removeMutation.mutate({
      mountpoint: removePlan.mountpoint,
      confirmation: removeConfirmValue,
    });
    if (!result.ok) {
      setRemoveError(result.error);
      setRemovePending(false);
      return;
    }
    setRemoveOpen(false);
    resetRemoveDialog();
    await refresh();
    setRemovePending(false);
  }

  const handleStop = async (): Promise<void> => {
    setPending(true);
    const result = await stopMutation.mutate(undefined);
    if (!result.ok) {
      setActionError(result.error);
      setPending(false);
      return;
    }
    setActionError(null);
    setStopOpen(false);
    await refresh();
    setPending(false);
  };

  const handleStart = async (): Promise<void> => {
    setPending(true);
    const result = await startMutation.mutate(undefined);
    if (!result.ok) {
      setActionError(result.error);
      setPending(false);
      return;
    }
    setActionError(null);
    setStartOpen(false);
    await refresh();
    setPending(false);
  };

  if (loading) {
    return <LoadingBlock />;
  }

  if (error && !pool) {
    return <Banner tone="error" title={error} />;
  }

  const disks = pool?.disks ?? [];
  // dataDisks is every data-role slot regardless of state (#326): a
  // missing one — the failed disk doc 02 §4 "Replacing a failed disk"
  // exists for — is exactly what the replace dialog's own slot list must
  // offer (finding 2), not only a slot whose disk is still present.
  const dataDisks = disks.filter((disk) => disk.role === "data");
  const unassignedDisks = disks.filter((disk) => disk.role === "unassigned");
  // upgradableDisks are the data and parity slots an upgrade (doc 02 §4)
  // can target — a missing slot has nothing to copy from and goes through
  // Replace disk instead.
  const upgradableDisks = disks.filter(
    (disk) => (disk.role === "data" || disk.role === "parity") && disk.state !== "missing" && disk.device,
  );
  const total = disks.reduce((sum, disk) => sum + (disk.sizeBytes ?? 0), 0);
  const used = disks.reduce((sum, disk) => sum + (disk.usedBytes ?? 0), 0);
  const usedPercent = total > 0 ? Math.round((used / total) * 100) : 0;

  const stopItems = [
    t("pool.stop.items.jobs"),
    t("pool.stop.items.vms"),
    t("pool.stop.items.containers"),
    t("pool.stop.items.shares"),
    t("pool.stop.items.mounts"),
  ];

  const addIdentity = addPlan ? diskIdentitySummary(t, addPlan) : null;
  const replaceIdentity = replacePlan ? diskIdentitySummary(t, replacePlan) : null;
  const upgradeIdentity = upgradePlan ? diskIdentitySummary(t, upgradePlan) : null;
  const upgradeIsParity = upgradePlan?.role === "parity";

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h1 className="text-2xl font-semibold font-heading">{t("storageNav.pool")}</h1>
          <p className="text-muted-foreground">{t("pool.description")}</p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" disabled={maintenance} onClick={() => setAddOpen(true)}>
            {t("pool.addDisk")}
          </Button>
          <Button variant="outline" disabled={maintenance} onClick={() => setReplaceOpen(true)}>
            {t("pool.replaceDisk")}
          </Button>
          {/* Unlike Add/Replace, Upgrade disk stays enabled in maintenance
              mode: a data-disk upgrade runs only with the array stopped
              (doc 02 §4). Every refusal — array_not_stopped,
              disk_upgrade_pending, or maintenance_mode for a parity
              upgrade — is shown inside the dialog. */}
          <Button variant="outline" onClick={() => setUpgradeOpen(true)}>
            {t("pool.upgradeDisk")}
          </Button>
          <Button
            variant="outline"
            disabled={maintenance}
            onClick={() => {
              setRebalanceOpen(true);
              void previewRebalance();
            }}
          >
            {t("pool.rebalance")}
          </Button>
          <Button variant="outline" disabled={maintenance} onClick={() => setRemoveOpen(true)}>
            {t("pool.removeDisk")}
          </Button>
          {maintenance ? (
            <Button variant="default" onClick={() => setStartOpen(true)}>
              {t("pool.startArray")}
            </Button>
          ) : (
            <Button variant="destructive-outline" onClick={() => setStopOpen(true)}>
              {t("pool.stopArray")}
            </Button>
          )}
        </div>
      </div>
      {error ? <Banner tone="error" title={error} /> : null}
      {actionError ? <Banner tone="error" title={actionError} /> : null}
      <InlineNote description={t("pool.noRebuildNote")} />
      <div className="grid gap-4 md:grid-cols-2">
        <MetricTile
          title={t("pool.capacity")}
          value={`${formatBytes(used)} / ${formatBytes(total)}`}
          description={t("pool.mountPoint", { path: "/mnt/user" })}
          progress={usedPercent}
          footer={
            <StatusBadge tone={pool?.mounted ? "success" : "error"}>
              {pool?.mounted ? t("pool.mounted") : t("pool.unmounted")}
            </StatusBadge>
          }
        />
        <MetricTile
          title={t("pool.diskCount")}
          value={String(dataDisks.length)}
          description={t("pool.dataDisks")}
        />
      </div>
      <section className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {disks.map((disk) => {
          const size = disk.sizeBytes ?? 0;
          const diskUsed = disk.usedBytes;
          const percent = diskUsed != null && size > 0 ? Math.round((diskUsed / size) * 100) : null;
          const missing = disk.state === "missing";
          return (
            <MetricTile
              key={`${disk.device || "missing"}-${disk.mountPoint}`}
              title={disk.mountPoint}
              value={disk.device}
              description={`${disk.role} · ${diskUsed == null ? t("pool.diskUsageUnavailable") : formatBytes(diskUsed)}`}
              progress={percent}
              footer={
                <StatusBadge tone={disk.state === "failed" || missing ? "error" : "success"}>
                  {t(`pool.diskState.${disk.state}`)}
                </StatusBadge>
              }
            />
          );
        })}
      </section>
      <ConfirmDialog
        open={stopOpen}
        onOpenChange={setStopOpen}
        title={t("pool.stop.title")}
        description={t("pool.stop.description")}
        items={stopItems}
        confirmLabel={t("pool.stop.confirm")}
        destructive
        loading={pending}
        onConfirm={() => void handleStop()}
      />
      <ConfirmDialog
        open={startOpen}
        onOpenChange={setStartOpen}
        title={t("pool.start.title")}
        description={t("pool.start.description")}
        confirmLabel={t("pool.start.confirm")}
        loading={pending}
        onConfirm={() => void handleStart()}
      />
      <FormOverlay
        open={addOpen}
        onOpenChange={(open) => {
          setAddOpen(open);
          if (!open) resetAddDialog();
        }}
        title={t("pool.add.title")}
        description={t("pool.add.description")}
        footer={
          addPlan ? (
            <Button
              variant="default"
              disabled={addPending || addConfirmValue !== addPlan.confirmation}
              onClick={() => void submitAddDisk()}
            >
              {t("pool.add.submit")}
            </Button>
          ) : (
            <Button variant="default" disabled={!addDevice || addPending} onClick={() => void previewAddDisk()}>
              {t("pool.add.preview")}
            </Button>
          )
        }
      >
        {addError ? <Banner tone="error" title={addError} /> : null}
        <Field>
          <FieldLabel>{t("pool.add.deviceLabel")}</FieldLabel>
          <SelectFilter
            value={addDevice}
            onChange={(value) => {
              addSelectionGen.current += 1;
              setAddDevice(value);
              resetAddPlan();
            }}
            placeholder={t("pool.add.devicePlaceholder")}
            options={unassignedDisks.map((disk) => ({ value: disk.device, label: disk.device }))}
          />
        </Field>
        <Field>
          <FieldLabel>{t("pool.add.filesystemLabel")}</FieldLabel>
          <SelectFilter
            value={addFilesystem}
            onChange={(value) => {
              addSelectionGen.current += 1;
              setAddFilesystem(value as ArrayDiskFilesystem);
              resetAddPlan();
            }}
            placeholder={t("pool.add.filesystemLabel")}
            options={FILESYSTEM_OPTIONS}
          />
        </Field>
        <div className="flex items-center gap-2">
          <Switch
            checked={addAdopt}
            onCheckedChange={(checked) => {
              addSelectionGen.current += 1;
              setAddAdopt(checked);
              resetAddPlan();
            }}
            aria-label={t("pool.add.adoptLabel")}
          />
          <span className="text-sm">{t("pool.add.adoptLabel")}</span>
        </div>
        {addPlan ? (
          <TypedConfirm
            phrase={addPlan.confirmation}
            value={addConfirmValue}
            onChange={setAddConfirmValue}
            title={t("pool.add.confirmTitle", { device: addPlan.device })}
            description={t("pool.add.confirmDescription", { mountpoint: addPlan.mountpoint })}
            items={[
              ...(addIdentity ? [addIdentity] : []),
              addPlan.adopt
                ? t("pool.add.adoptItem")
                : t("pool.add.formatItem", { filesystem: addPlan.filesystem }),
              t("pool.add.mountpointItem", { mountpoint: addPlan.mountpoint }),
            ]}
          />
        ) : null}
      </FormOverlay>
      <FormOverlay
        open={replaceOpen}
        onOpenChange={(open) => {
          setReplaceOpen(open);
          if (!open) resetReplaceDialog();
        }}
        title={t("pool.replace.title")}
        description={t("pool.replace.description")}
        footer={
          replacePlan ? (
            <Button
              variant="destructive"
              disabled={replacePending || replaceConfirmValue !== replacePlan.confirmation}
              onClick={() => void submitReplaceDisk()}
            >
              {t("pool.replace.submit")}
            </Button>
          ) : (
            <Button
              variant="default"
              disabled={!replaceMountpoint || !replaceDevice || replacePending}
              onClick={() => void previewReplaceDisk()}
            >
              {t("pool.replace.preview")}
            </Button>
          )
        }
      >
        {replaceError ? <Banner tone="error" title={replaceError} /> : null}
        <Field>
          <FieldLabel>{t("pool.replace.slotLabel")}</FieldLabel>
          <SelectFilter
            value={replaceMountpoint}
            onChange={(value) => {
              replaceSelectionGen.current += 1;
              setReplaceMountpoint(value);
              resetReplacePlan();
            }}
            placeholder={t("pool.replace.slotPlaceholder")}
            options={dataDisks.map((disk) => ({ value: disk.mountPoint, label: slotLabel(t, disk) }))}
          />
        </Field>
        <Field>
          <FieldLabel>{t("pool.replace.deviceLabel")}</FieldLabel>
          <SelectFilter
            value={replaceDevice}
            onChange={(value) => {
              replaceSelectionGen.current += 1;
              setReplaceDevice(value);
              resetReplacePlan();
            }}
            placeholder={t("pool.replace.devicePlaceholder")}
            options={unassignedDisks.map((disk) => ({ value: disk.device, label: disk.device }))}
          />
        </Field>
        <Field>
          <FieldLabel>{t("pool.replace.filesystemLabel")}</FieldLabel>
          <SelectFilter
            value={replaceFilesystem}
            onChange={(value) => {
              replaceSelectionGen.current += 1;
              setReplaceFilesystem(value as ArrayDiskFilesystem);
              resetReplacePlan();
            }}
            placeholder={t("pool.replace.filesystemLabel")}
            options={FILESYSTEM_OPTIONS}
          />
        </Field>
        <div className="flex items-center gap-2">
          <Switch
            checked={replaceAdopt}
            onCheckedChange={(checked) => {
              replaceSelectionGen.current += 1;
              setReplaceAdopt(checked);
              resetReplacePlan();
            }}
            aria-label={t("pool.replace.adoptLabel")}
          />
          <span className="text-sm">{t("pool.replace.adoptLabel")}</span>
        </div>
        {replacePlan ? (
          <TypedConfirm
            phrase={replacePlan.confirmation}
            value={replaceConfirmValue}
            onChange={setReplaceConfirmValue}
            title={t("pool.replace.confirmTitle", { mountpoint: replacePlan.mountpoint })}
            description={t("pool.replace.confirmDescription", { device: replacePlan.replacementDevice })}
            items={[
              ...(replaceIdentity ? [replaceIdentity] : []),
              replacePlan.adopt
                ? t("pool.replace.adoptItem")
                : t("pool.replace.formatItem", { filesystem: replacePlan.filesystem }),
              t("pool.replace.rebuildItem", { rebuild: replacePlan.rebuild }),
            ]}
          />
        ) : null}
      </FormOverlay>
      <FormOverlay
        open={upgradeOpen}
        onOpenChange={(open) => {
          setUpgradeOpen(open);
          if (!open) resetUpgradeDialog();
        }}
        title={t("pool.upgrade.title")}
        description={t("pool.upgrade.description")}
        footer={
          upgradePlan ? (
            <Button
              variant="destructive"
              disabled={upgradePending || upgradeConfirmValue !== upgradePlan.confirmation}
              onClick={() => void submitUpgradeDisk()}
            >
              {t("pool.upgrade.submit")}
            </Button>
          ) : (
            <Button
              variant="default"
              disabled={!upgradeMountpoint || !upgradeDevice || upgradePending}
              onClick={() => void previewUpgradeDisk()}
            >
              {t("pool.upgrade.preview")}
            </Button>
          )
        }
      >
        {upgradeError ? <Banner tone="error" title={upgradeError} /> : null}
        <Field>
          <FieldLabel>{t("pool.upgrade.slotLabel")}</FieldLabel>
          <SelectFilter
            value={upgradeMountpoint}
            onChange={(value) => {
              upgradeSelectionGen.current += 1;
              setUpgradeMountpoint(value);
              resetUpgradePlan();
            }}
            placeholder={t("pool.upgrade.slotPlaceholder")}
            options={upgradableDisks.map((disk) => ({ value: disk.mountPoint, label: upgradeSlotLabel(disk) }))}
          />
        </Field>
        <Field>
          <FieldLabel>{t("pool.upgrade.deviceLabel")}</FieldLabel>
          <SelectFilter
            value={upgradeDevice}
            onChange={(value) => {
              upgradeSelectionGen.current += 1;
              setUpgradeDevice(value);
              resetUpgradePlan();
            }}
            placeholder={t("pool.upgrade.devicePlaceholder")}
            options={unassignedDisks.map((disk) => ({ value: disk.device, label: disk.device }))}
          />
        </Field>
        <Field>
          <FieldLabel>{t("pool.upgrade.filesystemLabel")}</FieldLabel>
          <SelectFilter
            value={upgradeFilesystem}
            onChange={(value) => {
              upgradeSelectionGen.current += 1;
              setUpgradeFilesystem(value as ArrayDiskFilesystem);
              resetUpgradePlan();
            }}
            placeholder={t("pool.upgrade.filesystemLabel")}
            options={FILESYSTEM_OPTIONS}
          />
        </Field>
        {upgradePlan ? (
          <TypedConfirm
            phrase={upgradePlan.confirmation}
            value={upgradeConfirmValue}
            onChange={setUpgradeConfirmValue}
            title={t("pool.upgrade.confirmTitle", { mountpoint: upgradePlan.mountpoint })}
            description={t("pool.upgrade.confirmDescription", { device: upgradePlan.replacementDevice })}
            items={[
              ...(upgradeIdentity ? [upgradeIdentity] : []),
              t("pool.upgrade.formatItem", { filesystem: upgradePlan.filesystem }),
              ...(upgradeIsParity && upgradePlan.newMountpoint
                ? [t("pool.upgrade.newMountpointItem", { mountpoint: upgradePlan.newMountpoint })]
                : []),
              ...upgradePlan.steps.map((step) => t("pool.upgrade.stepItem", { step })),
            ]}
          />
        ) : null}
      </FormOverlay>
      <FormOverlay
        open={rebalanceOpen}
        onOpenChange={(open) => {
          setRebalanceOpen(open);
          if (!open) resetRebalanceDialog();
        }}
        title={t("pool.rebalanceDialog.title")}
        description={t("pool.rebalanceDialog.description")}
        footer={
          rebalancePlan && rebalancePlan.moves.length > 0 ? (
            <Button
              variant="default"
              disabled={rebalancePending || rebalanceConfirmValue !== rebalancePlan.confirmation}
              onClick={() => void submitRebalance()}
            >
              {t("pool.rebalanceDialog.submit")}
            </Button>
          ) : undefined
        }
      >
        {rebalanceError ? <Banner tone="error" title={rebalanceError} /> : null}
        {rebalancePending && !rebalancePlan ? <LoadingBlock /> : null}
        {rebalancePlan && rebalancePlan.moves.length === 0 ? (
          <InlineNote description={t("pool.rebalanceDialog.alreadyBalanced")} />
        ) : null}
        {rebalancePlan && rebalancePlan.moves.length > 0 ? (
          <TypedConfirm
            phrase={rebalancePlan.confirmation}
            value={rebalanceConfirmValue}
            onChange={setRebalanceConfirmValue}
            title={t("pool.rebalanceDialog.confirmTitle")}
            description={t("pool.rebalanceDialog.confirmDescription")}
            items={rebalancePlanItems(t, rebalancePlan)}
          />
        ) : null}
      </FormOverlay>
      <FormOverlay
        open={removeOpen}
        onOpenChange={(open) => {
          setRemoveOpen(open);
          if (!open) resetRemoveDialog();
        }}
        title={t("pool.remove.title")}
        description={t("pool.remove.description")}
        footer={
          removePlan ? (
            <Button
              variant="destructive"
              disabled={removePending || removeConfirmValue !== removePlan.confirmation}
              onClick={() => void submitRemoveDisk()}
            >
              {t("pool.remove.submit")}
            </Button>
          ) : (
            <Button
              variant="default"
              disabled={!removeMountpoint || removePending}
              onClick={() => void previewRemoveDisk()}
            >
              {t("pool.remove.preview")}
            </Button>
          )
        }
      >
        {removeError ? <Banner tone="error" title={removeError} /> : null}
        <Field>
          <FieldLabel>{t("pool.remove.slotLabel")}</FieldLabel>
          <SelectFilter
            value={removeMountpoint}
            onChange={(value) => {
              removeSelectionGen.current += 1;
              setRemoveMountpoint(value);
              resetRemovePlan();
            }}
            placeholder={t("pool.remove.slotPlaceholder")}
            options={dataDisks.map((disk) => ({ value: disk.mountPoint, label: slotLabel(t, disk) }))}
          />
        </Field>
        {removePlan ? (
          <TypedConfirm
            phrase={removePlan.confirmation}
            value={removeConfirmValue}
            onChange={setRemoveConfirmValue}
            title={t("pool.remove.confirmTitle", { mountpoint: removePlan.mountpoint })}
            description={t("pool.remove.confirmDescription")}
            items={rebalancePlanItems(t, removePlan)}
          />
        ) : null}
      </FormOverlay>
    </div>
  );
}
