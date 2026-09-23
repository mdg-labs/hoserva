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
import { hoservaClient, type components } from "@/lib/api/client";
import { formatBytes } from "@/routes/storage-setup/config-preview";
import type { TFunction } from "i18next";

type AddDiskPlan = components["schemas"]["AddDiskPlan"];
type ReplaceDiskPlan = components["schemas"]["ReplaceDiskPlan"];
type DiskUpgradePlan = components["schemas"]["DiskUpgradePlan"];
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
    try {
      const { data, error: apiError } = await hoservaClient.POST("/disks/array/add/plan", {
        body: { device, filesystem, adopt },
      });
      // The selection moved on while this request was in flight — never
      // install a plan (or an error) for a device the dialog no longer
      // shows (finding 4).
      if (addSelectionGen.current !== gen) return;
      if (apiError) {
        setAddError(apiError.message);
        return;
      }
      setAddPlan(data ?? null);
      setAddConfirmValue("");
    } catch (err: unknown) {
      if (addSelectionGen.current !== gen) return;
      setAddError(err instanceof Error ? err.message : String(err));
    } finally {
      if (addSelectionGen.current === gen) setAddPending(false);
    }
  }

  async function submitAddDisk(): Promise<void> {
    if (!addPlan) return;
    setAddPending(true);
    setAddError(null);
    try {
      // Submit the plan's own fields, never the live form state (finding
      // 4): the plan is what the operator actually previewed and typed
      // the confirmation phrase against.
      const { error: apiError } = await hoservaClient.POST("/disks/array/add", {
        body: {
          device: addPlan.device,
          filesystem: addPlan.filesystem,
          adopt: addPlan.adopt,
          confirmation: addConfirmValue,
        },
      });
      if (apiError) {
        setAddError(apiError.message);
        return;
      }
      setAddOpen(false);
      resetAddDialog();
      await refresh();
    } catch (err: unknown) {
      setAddError(err instanceof Error ? err.message : String(err));
    } finally {
      setAddPending(false);
    }
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
    try {
      const { data, error: apiError } = await hoservaClient.POST("/disks/array/replace/plan", {
        body: { mountpoint, device, filesystem, adopt },
      });
      if (replaceSelectionGen.current !== gen) return;
      if (apiError) {
        setReplaceError(apiError.message);
        return;
      }
      setReplacePlan(data ?? null);
      setReplaceConfirmValue("");
    } catch (err: unknown) {
      if (replaceSelectionGen.current !== gen) return;
      setReplaceError(err instanceof Error ? err.message : String(err));
    } finally {
      if (replaceSelectionGen.current === gen) setReplacePending(false);
    }
  }

  async function submitReplaceDisk(): Promise<void> {
    if (!replacePlan) return;
    setReplacePending(true);
    setReplaceError(null);
    try {
      // Submit the plan's own fields, never the live form state (finding
      // 4) — the slot and device the operator actually previewed.
      const { error: apiError } = await hoservaClient.POST("/disks/array/replace", {
        body: {
          mountpoint: replacePlan.mountpoint,
          device: replacePlan.replacementDevice,
          filesystem: replacePlan.filesystem,
          adopt: replacePlan.adopt,
          confirmation: replaceConfirmValue,
        },
      });
      if (apiError) {
        setReplaceError(apiError.message);
        return;
      }
      setReplaceOpen(false);
      resetReplaceDialog();
      await refresh();
    } catch (err: unknown) {
      setReplaceError(err instanceof Error ? err.message : String(err));
    } finally {
      setReplacePending(false);
    }
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
    try {
      const { data, error: apiError } = await hoservaClient.POST("/disks/array/upgrade/plan", {
        body: { mountpoint, device, filesystem },
      });
      // The selection moved on while this request was in flight — never
      // install a plan (or an error) for a slot the dialog no longer shows.
      if (upgradeSelectionGen.current !== gen) return;
      if (apiError) {
        setUpgradeError(apiError.message);
        return;
      }
      setUpgradePlan(data ?? null);
      setUpgradeConfirmValue("");
    } catch (err: unknown) {
      if (upgradeSelectionGen.current !== gen) return;
      setUpgradeError(err instanceof Error ? err.message : String(err));
    } finally {
      if (upgradeSelectionGen.current === gen) setUpgradePending(false);
    }
  }

  async function submitUpgradeDisk(): Promise<void> {
    if (!upgradePlan) return;
    setUpgradePending(true);
    setUpgradeError(null);
    try {
      // Submit the plan's own fields, never the live form state — the
      // slot, device and (for a parity slot) fresh mountpoint the operator
      // actually previewed and typed the confirmation against.
      const { error: apiError } = await hoservaClient.POST("/disks/array/upgrade", {
        body: {
          mountpoint: upgradePlan.mountpoint,
          device: upgradePlan.replacementDevice,
          filesystem: upgradePlan.filesystem,
          newMountpoint: upgradePlan.newMountpoint,
          confirmation: upgradeConfirmValue,
        },
      });
      if (apiError) {
        setUpgradeError(apiError.message);
        return;
      }
      setUpgradeOpen(false);
      resetUpgradeDialog();
      await refresh();
    } catch (err: unknown) {
      setUpgradeError(err instanceof Error ? err.message : String(err));
    } finally {
      setUpgradePending(false);
    }
  }

  const handleStop = async (): Promise<void> => {
    setPending(true);
    try {
      const { error: apiError } = await hoservaClient.POST("/array/stop", {
        body: { confirm: true },
      });
      if (apiError) {
        setActionError(apiError.message);
        return;
      }
      setActionError(null);
      setStopOpen(false);
      await refresh();
    } catch (err: unknown) {
      setActionError(err instanceof Error ? err.message : String(err));
    } finally {
      setPending(false);
    }
  };

  const handleStart = async (): Promise<void> => {
    setPending(true);
    try {
      const { error: apiError } = await hoservaClient.POST("/array/start");
      if (apiError) {
        setActionError(apiError.message);
        return;
      }
      setActionError(null);
      setStartOpen(false);
      await refresh();
    } catch (err: unknown) {
      setActionError(err instanceof Error ? err.message : String(err));
    } finally {
      setPending(false);
    }
  };

  if (loading) {
    return <LoadingBlock />;
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
    </div>
  );
}
