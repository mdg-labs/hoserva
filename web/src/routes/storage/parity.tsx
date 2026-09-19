import { useEffect, useState } from "react";
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
import { hoservaClient, type components } from "@/lib/api/client";
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
  const [parity, setParity] = useState<ParitySnapshot | null>(null);
  const [parityLoaded, setParityLoaded] = useState(false);
  const [parityError, setParityError] = useState<string | null>(null);
  const [diffGroups, setDiffGroups] = useState<ParityDiffGroup[]>([]);
  const [runDiffOpen, setRunDiffOpen] = useState(false);
  const [runDiffBusy, setRunDiffBusy] = useState(false);
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

  useEffect(() => {
    const controller = new AbortController();
    hoservaClient
      .GET("/parity", { signal: controller.signal })
      .then(({ data, error: apiError }) => {
        if (controller.signal.aborted) {
          return;
        }
        if (apiError) {
          setParityError(apiError.message);
          setParity(null);
          setDiffGroups([]);
          return;
        }
        setParityError(null);
        setParity(data ?? null);
        if (data?.groups?.length) {
          setDiffGroups(parityDiffGroupsFromAPI(data.groups));
        }
      })
      .catch((err: unknown) => {
        if (!controller.signal.aborted) {
          setParityError(err instanceof Error ? err.message : String(err));
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) {
          setParityLoaded(true);
        }
      });
    return () => {
      controller.abort();
    };
  }, []);

  const parityDisk = pool?.disks.find((disk) => disk.role === "parity");
  const guard = parity?.guard;
  const guardTripped = guard?.wouldBlock ?? Boolean(status?.parityBlocked);
  const guardSummary = guard?.summary;
  const parityJobs = jobs.filter((job) => job.class === "parity");

  const handleRunDiff = async (): Promise<void> => {
    setRunDiffBusy(true);
    try {
      const { data, error: apiError } = await hoservaClient.POST("/parity/diff");
      if (apiError) {
        setActionError(apiError.message);
        return;
      }
      setActionError(null);
      setRunDiffOpen(false);
      if (data) {
        setDiffGroups(parityDiffGroupsFromAPI(data.groups));
        setParity((current) =>
          current
            ? {
                ...current,
                guard: data.guard,
                groups: data.groups,
              }
            : {
                freshness: "green",
                guard: data.guard,
                groups: data.groups,
              },
        );
      }
    } catch (err: unknown) {
      setActionError(err instanceof Error ? err.message : String(err));
    } finally {
      setRunDiffBusy(false);
    }
  };

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
      const parityRefresh = await hoservaClient.GET("/parity");
      if (!parityRefresh.error && parityRefresh.data) {
        setParity(parityRefresh.data);
        if (parityRefresh.data.groups?.length) {
          setDiffGroups(parityDiffGroupsFromAPI(parityRefresh.data.groups));
        }
      }
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

  if (loading || !parityLoaded) {
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
        loading={runDiffBusy}
        onConfirm={() => void handleRunDiff()}
      />
      <ConfirmDialog
        open={syncOpen}
        onOpenChange={setSyncOpen}
        title={t("parity.actions.sync")}
        description={guardTripped ? guardSummary ?? t("parity.guard.bannerDescription") : t("parity.actions.syncDescription")}
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
