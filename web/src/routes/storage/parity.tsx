import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { ConfirmDialog } from "@/components/patterns/confirm";
import { GroupedResults } from "@/components/patterns/grouped-results";
import { InlineNote } from "@/components/patterns/inline-note";
import { LoadingBlock } from "@/components/patterns/loading";
import {
  parityDiffGroupsFromAPI,
  type ParityDiffGroup,
} from "@/components/patterns/parity-diff";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Wizard } from "@/components/patterns/wizard";
import { useUnsavedGuard } from "@/hooks/use-unsaved-guard";
import { useSystemData } from "@/hooks/use-system-status";
import {
  getParity,
  postParityDiff,
  postParityFix,
  postParityScrub,
  postParitySync,
} from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import type { components } from "@/lib/api/client";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
} from "@/components/ui/dialog";

type ParitySnapshot = components["schemas"]["ParitySnapshot"];

function freshnessTone(freshness: ParitySnapshot["freshness"]): "success" | "warning" | "error" {
  switch (freshness) {
    case "red":
      return "error";
    case "amber":
      return "warning";
    default:
      return "success";
  }
}

export function ParityPage(): React.ReactElement {
  const { t } = useTranslation();
  const { status, pool, doctor, jobs, loading, error, refresh } = useSystemData();
  const parityQuery = useApiQuery<ParitySnapshot>({
    queryKey: "parity-snapshot",
    queryFn: (signal) => getParity(signal),
  });
  const diffMutation = useApiMutation({ mutationFn: () => postParityDiff() });
  const syncMutation = useApiMutation({ mutationFn: () => postParitySync() });
  const fixMutation = useApiMutation({ mutationFn: () => postParityFix() });
  const scrubMutation = useApiMutation({ mutationFn: () => postParityScrub() });

  const parity = parityQuery.data;
  const parityError = parityQuery.error;
  const parityDiffGroups = useMemo(
    () => (parity?.groups?.length ? parityDiffGroupsFromAPI(parity.groups) : []),
    [parity],
  );
  const [localDiffGroups, setLocalDiffGroups] = useState<ParityDiffGroup[] | null>(null);
  const diffGroups = localDiffGroups ?? parityDiffGroups;
  const [runDiffOpen, setRunDiffOpen] = useState(false);
  const [syncOpen, setSyncOpen] = useState(false);
  const [fixOpen, setFixOpen] = useState(false);
  const [fixStep, setFixStep] = useState(0);
  const [fixDirty, setFixDirty] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [diffDialogError, setDiffDialogError] = useState<string | null>(null);
  const [syncDialogError, setSyncDialogError] = useState<string | null>(null);
  const { requestClose, guardDialog } = useUnsavedGuard({
    dirty: fixDirty,
    onClose: () => {
      setFixOpen(false);
      setFixStep(0);
      setFixDirty(false);
    },
  });

  const parityDisk = pool?.disks.find((disk) => disk.role === "parity");
  const guard = parity?.guard;
  const guardTripped = guard?.wouldBlock ?? Boolean(status?.parityBlocked);
  const guardSummary = guard?.summary;
  const parityJobs = jobs.filter((job) => job.class === "parity");

  const handleRunDiff = async (): Promise<void> => {
    const result = await diffMutation.mutate(undefined);
    if (!result.ok) {
      setDiffDialogError(result.error);
      return;
    }
    setDiffDialogError(null);
    setRunDiffOpen(false);
    if (result.data) {
      setLocalDiffGroups(parityDiffGroupsFromAPI(result.data.groups));
      await parityQuery.refresh();
    }
  };

  const handleSync = async (): Promise<void> => {
    const result = await syncMutation.mutate(undefined);
    if (!result.ok) {
      setSyncDialogError(result.error);
      return;
    }
    setSyncDialogError(null);
    setSyncOpen(false);
    await refresh();
    await parityQuery.refresh();
  };

  const handleFixStart = async (): Promise<void> => {
    const result = await fixMutation.mutate(undefined);
    if (!result.ok) {
      setActionError(result.error);
      return;
    }
    setActionError(null);
    setFixOpen(false);
    setFixStep(0);
    setFixDirty(false);
    await refresh();
  };

  const handleScrub = async (): Promise<void> => {
    const result = await scrubMutation.mutate(undefined);
    if (!result.ok) {
      setActionError(result.error);
      return;
    }
    setActionError(null);
    await refresh();
  };

  if (loading || parityQuery.loading) {
    return <LoadingBlock />;
  }

  const freshnessMessage =
    doctor?.checks.find((check) => check.id === "parity_freshness")?.message ?? "—";

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-2xl font-semibold font-heading">{t("storageNav.parity")}</h1>
        <p className="text-muted-foreground">{t("parity.description")}</p>
      </div>
      {error ? <Banner tone="error" title={error} /> : null}
      {parityError ? <Banner tone="error" title={parityError} /> : null}
      {actionError ? <Banner tone="error" title={actionError} /> : null}
      <Card>
        <CardHeader>
          <CardTitle>{t("parity.status.title")}</CardTitle>
        </CardHeader>
        <CardPanel className="grid gap-2 text-sm sm:grid-cols-2">
          <p>{t("parity.status.disk", { device: parityDisk?.device ?? "—" })}</p>
          <p>{freshnessMessage}</p>
          <StatusBadge tone={parity ? freshnessTone(parity.freshness) : "outline"}>
            {freshnessMessage}
          </StatusBadge>
          <StatusBadge tone={guardTripped ? "error" : "success"}>
            {guardTripped ? t("parity.guard.blocked") : t("parity.guard.clear")}
          </StatusBadge>
        </CardPanel>
      </Card>
      {guardTripped ? (
        <Banner
          tone="error"
          title={t("parity.guard.bannerTitle")}
          description={guardSummary ?? t("parity.guard.bannerDescription")}
        />
      ) : null}
      <section className="flex flex-col gap-3">
        <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <h2 className="font-medium">{t("parity.diff.title")}</h2>
            <p className="text-muted-foreground text-sm">{t("parity.diff.runDiffWarning")}</p>
          </div>
          <Button variant="outline" onClick={() => setRunDiffOpen(true)}>
            {t("parity.diff.runDiff")}
          </Button>
        </div>
        {diffGroups.length === 0 ? (
          <InlineNote description={t("parity.diff.empty")} />
        ) : (
          <GroupedResults
            groups={diffGroups.map((group) => ({
              id: group.category,
              label: t(`parity.diff.categories.${group.category}`),
              count: group.count,
              items: group.paths.length > 0 ? group.paths : ["—"],
              defaultOpen: group.category === "removed",
            }))}
          />
        )}
      </section>
      <div className="flex flex-wrap gap-2">
        <Button onClick={() => setSyncOpen(true)}>{t("parity.actions.sync")}</Button>
        <Button variant="outline" onClick={() => void handleScrub()}>
          {t("parity.actions.scrub")}
        </Button>
        <Button variant="outline" onClick={() => setFixOpen(true)}>
          {t("parity.actions.fix")}
        </Button>
      </div>
      <Card>
        <CardHeader>
          <CardTitle>{t("parity.history.title")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-2 text-sm">
          {parityJobs.length === 0 ? (
            <p className="text-muted-foreground">{t("parity.history.empty")}</p>
          ) : (
            parityJobs.map((job) => (
              <div key={job.id} data-job-id={job.id} className="flex items-center justify-between gap-2">
                <span>{job.type}</span>
                <StatusBadge tone={job.status === "failed" ? "error" : job.status === "succeeded" ? "success" : "info"}>
                  {job.status}
                </StatusBadge>
              </div>
            ))
          )}
        </CardPanel>
      </Card>
      <ConfirmDialog
        open={runDiffOpen}
        onOpenChange={(open) => {
          setRunDiffOpen(open);
          if (!open) {
            setDiffDialogError(null);
          }
        }}
        title={t("parity.diff.runDiff")}
        description={
          <>
            {t("parity.diff.runDiffWarning")}
            {diffDialogError ? <Banner tone="error" title={diffDialogError} /> : null}
          </>
        }
        confirmLabel={t("parity.diff.runDiffConfirm")}
        loading={diffMutation.pending}
        onConfirm={() => void handleRunDiff()}
      />
      <ConfirmDialog
        open={syncOpen}
        onOpenChange={(open) => {
          setSyncOpen(open);
          if (!open) {
            setSyncDialogError(null);
          }
        }}
        title={t("parity.actions.sync")}
        description={
          <>
            {guardTripped ? guardSummary ?? t("parity.guard.bannerDescription") : t("parity.actions.syncDescription")}
            {syncDialogError ? <Banner tone="error" title={syncDialogError} /> : null}
          </>
        }
        confirmLabel={t("parity.actions.sync")}
        onConfirm={() => void handleSync()}
      />
      <Dialog open={fixOpen} onOpenChange={(open) => !open && requestClose()}>
        <DialogPopup className="max-w-2xl">
          <DialogHeader>
            <DialogTitle>{t("parity.fix.title")}</DialogTitle>
          </DialogHeader>
          <DialogPanel>
            <Wizard
              step={fixStep}
              stepCount={4}
              title={t(`parity.fix.steps.${fixStep}.title`)}
              description={t(`parity.fix.steps.${fixStep}.description`)}
              onBack={fixStep > 0 ? () => setFixStep((step) => step - 1) : undefined}
              onNext={() => {
                if (fixStep >= 3) {
                  void handleFixStart();
                  return;
                }
                setFixStep((step) => step + 1);
              }}
              nextLabel={fixStep >= 3 ? t("parity.fix.execute") : undefined}
            >
              {fixStep === 2 ? (
                <Banner tone="warning" title={t("parity.fix.unrecoverableTitle")} description={t("parity.fix.unrecoverableDescription")} />
              ) : null}
            </Wizard>
          </DialogPanel>
        </DialogPopup>
      </Dialog>
      {guardDialog}
    </div>
  );
}
