import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { ConfirmDialog } from "@/components/patterns/confirm";
import { InlineNote } from "@/components/patterns/inline-note";
import { LoadingBlock } from "@/components/patterns/loading";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Wizard } from "@/components/patterns/wizard";
import { useUnsavedGuard } from "@/hooks/use-unsaved-guard";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
} from "@/components/ui/dialog";
import { useSystemData } from "@/hooks/use-system-status";
import { hoservaClient } from "@/lib/api/client";

export function ParityPage(): React.ReactElement {
  const { t } = useTranslation();
  const { status, pool, doctor, jobs, loading, error, refresh } = useSystemData();
  const [runDiffOpen, setRunDiffOpen] = useState(false);
  const [syncOpen, setSyncOpen] = useState(false);
  const [fixOpen, setFixOpen] = useState(false);
  const [fixStep, setFixStep] = useState(0);
  const [fixDirty, setFixDirty] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const { requestClose, guardDialog } = useUnsavedGuard({
    dirty: fixDirty,
    onClose: () => {
      setFixOpen(false);
      setFixStep(0);
      setFixDirty(false);
    },
  });

  const parityDisk = pool?.disks.find((disk) => disk.role === "parity");
  const guardTripped = Boolean(status?.parityBlocked);
  const parityJobs = jobs.filter((job) => job.class === "parity");

  const handleSync = async (): Promise<void> => {
    try {
      const { error: apiError } = await hoservaClient.POST("/parity/sync", {
        body: { confirm: true, dryRun: false },
      });
      if (apiError) {
        setActionError(apiError.message);
        return;
      }
      setActionError(null);
      setSyncOpen(false);
      await refresh();
    } catch (err: unknown) {
      setActionError(err instanceof Error ? err.message : String(err));
    }
  };

  const handleFixStart = async (): Promise<void> => {
    try {
      const { error: apiError } = await hoservaClient.POST("/parity/fix", { body: { confirm: true } });
      if (apiError) {
        setActionError(apiError.message);
        return;
      }
      setActionError(null);
      setFixOpen(false);
      setFixStep(0);
      setFixDirty(false);
      await refresh();
    } catch (err: unknown) {
      setActionError(err instanceof Error ? err.message : String(err));
    }
  };

  const handleScrub = async (): Promise<void> => {
    try {
      const { error: apiError } = await hoservaClient.POST("/parity/scrub", { body: { percent: 100 } });
      if (apiError) {
        setActionError(apiError.message);
        return;
      }
      setActionError(null);
      await refresh();
    } catch (err: unknown) {
      setActionError(err instanceof Error ? err.message : String(err));
    }
  };

  if (loading) {
    return <LoadingBlock />;
  }

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-2xl font-semibold font-heading">{t("storageNav.parity")}</h1>
        <p className="text-muted-foreground">{t("parity.description")}</p>
      </div>
      {error ? <Banner tone="error" title={error} /> : null}
      {actionError ? <Banner tone="error" title={actionError} /> : null}
      <Card>
        <CardHeader>
          <CardTitle>{t("parity.status.title")}</CardTitle>
        </CardHeader>
        <CardPanel className="grid gap-2 text-sm sm:grid-cols-2">
          <p>{t("parity.status.disk", { device: parityDisk?.device ?? "—" })}</p>
          <p>{doctor?.checks.find((check) => check.id === "parity_freshness")?.message ?? "—"}</p>
          <StatusBadge tone={guardTripped ? "error" : "success"}>
            {guardTripped ? t("parity.guard.blocked") : t("parity.guard.clear")}
          </StatusBadge>
        </CardPanel>
      </Card>
      {guardTripped ? (
        <Banner tone="error" title={t("parity.guard.bannerTitle")} description={t("parity.guard.bannerDescription")} />
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
        <InlineNote description={t("parity.diff.empty")} />
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
              <div key={job.id} className="flex items-center justify-between gap-2">
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
        onOpenChange={setRunDiffOpen}
        title={t("parity.diff.runDiff")}
        description={t("parity.diff.runDiffWarning")}
        confirmLabel={t("parity.diff.runDiffConfirm")}
        onConfirm={() => setRunDiffOpen(false)}
      />
      <ConfirmDialog
        open={syncOpen}
        onOpenChange={setSyncOpen}
        title={t("parity.actions.sync")}
        description={guardTripped ? t("parity.guard.bannerDescription") : t("parity.actions.syncDescription")}
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
