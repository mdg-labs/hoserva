import { Boxes, Play, Square } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { ConfirmDialog } from "@/components/patterns/confirm";
import { CopyValue } from "@/components/patterns/copy-value";
import { EmptyState } from "@/components/patterns/empty-state";
import { showFeedbackToast } from "@/components/patterns/feedback-toast";
import { LoadingBlock } from "@/components/patterns/loading";
import { SegmentedChoice } from "@/components/patterns/segmented-choice";
import { Button, buttonVariants } from "@/components/ui/button";
import { useIsMobile } from "@/hooks/use-media-query";
import { useSystemData } from "@/hooks/use-system-status";
import type { components } from "@/lib/api/client";
import { getAppUpdates, getApps, postAppRestart, postAppStart, postAppStop } from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import { ContainerView } from "@/routes/apps/container-view";
import {
  canStart,
  canStop,
  START,
  STOP,
  type App,
  type AppUpdate,
  type LifecycleAction,
} from "@/routes/apps/containers";
import { LogsPanel } from "@/routes/apps/logs-panel";

type ListAppsOK = components["schemas"]["ListAppsOK"];
type ListAppUpdatesOK = components["schemas"]["ListAppUpdatesOK"];
type DoctorCheck = components["schemas"]["DoctorCheck"];

type ViewMode = "cards" | "table";
type BulkAction = "start" | "stop";
type BulkPlan = { action: BulkAction; targets: App[] };
type Failure = { name: string; message: string };

const POLL_MS = 15_000;
const CARDS = "cards" as const;
const TABLE = "table" as const;
const VIEW_FIELD = "apps-view";
const CATALOG_PATH = "/apps/catalog";

// The install commands come from the daemon's own doctor check, never from
// text written here.
function isDockerCheck(check: DoctorCheck): boolean {
  return (check.id === "docker" || check.id === "docker_compose") && check.status !== "pass";
}

function DockerBanner({ message, checks }: { message?: string; checks: DoctorCheck[] }): React.ReactElement {
  const { t } = useTranslation();
  const remediations = checks.filter((check) => isDockerCheck(check) && check.remediation);
  return (
    <Banner
      tone="warning"
      title={t("apps.docker.title")}
      description={
        <div className="flex flex-col gap-3">
          <p>{message ?? t("apps.docker.description")}</p>
          {remediations.map((check) => (
            <div key={check.id} className="flex flex-col gap-1">
              <span className="font-medium">{check.name}</span>
              <CopyValue value={check.remediation ?? ""} label={check.name} />
            </div>
          ))}
        </div>
      }
    />
  );
}

function bulkTargets(apps: App[], action: BulkAction): App[] {
  return apps.filter((app) => (action === "start" ? canStart(app.state) : canStop(app.state)));
}

export function AppsPage(): React.ReactElement {
  const { t } = useTranslation();
  const isMobile = useIsMobile();
  const { doctor } = useSystemData();
  const [chosenView, setChosenView] = useState<ViewMode | null>(null);
  const [busy, setBusy] = useState<Record<string, LifecycleAction>>({});
  const [bulk, setBulk] = useState<BulkPlan | null>(null);
  const [bulkRunning, setBulkRunning] = useState(false);
  const [failures, setFailures] = useState<Failure[]>([]);
  const [logsApp, setLogsApp] = useState<App | null>(null);

  const appsQuery = useApiQuery<ListAppsOK>({
    queryKey: "apps",
    queryFn: (signal) => getApps(signal),
    pollIntervalMs: POLL_MS,
    fallbackError: t("apps.installed.loadFailed"),
  });
  const updatesQuery = useApiQuery<ListAppUpdatesOK>({
    queryKey: "app-updates",
    queryFn: (signal) => getAppUpdates(signal),
    fallbackError: t("apps.update.loadFailed"),
  });
  const lifecycle = useApiMutation<{ id: string; action: LifecycleAction }, App>({
    mutationFn: ({ id, action }) => {
      if (action === "start") return postAppStart(id);
      if (action === "stop") return postAppStop(id);
      return postAppRestart(id);
    },
    fallbackError: t("apps.errors.actionFailed"),
  });

  const list = appsQuery.data;
  const apps = useMemo(() => list?.apps ?? [], [list]);
  const updates = useMemo(() => {
    const data = updatesQuery.data;
    if (!data || !data.available) {
      return null;
    }
    return new Map<string, AppUpdate>(data.updates.map((update) => [update.container, update]));
  }, [updatesQuery.data]);

  const view: ViewMode = chosenView ?? (isMobile ? "cards" : "table");
  const locked = bulkRunning;
  const startable = bulkTargets(apps, "start");
  const stoppable = bulkTargets(apps, "stop");

  // Returns the failure message, or null when the Engine accepted the action.
  async function runAction(app: App, action: LifecycleAction): Promise<string | null> {
    setBusy((current) => ({ ...current, [app.id]: action }));
    try {
      const result = await lifecycle.mutate({ id: app.id, action });
      if (result.ok || result.aborted) {
        return null;
      }
      return result.error;
    } finally {
      setBusy((current) => {
        const next = { ...current };
        delete next[app.id];
        return next;
      });
    }
  }

  async function handleAction(app: App, action: LifecycleAction): Promise<void> {
    setFailures([]);
    const message = await runAction(app, action);
    if (message === null) {
      showFeedbackToast({ type: "success", title: t(`apps.toast.${action}Done`, { name: app.name }) });
    } else {
      setFailures([{ name: app.name, message }]);
      showFeedbackToast({
        type: "error",
        title: t(`apps.toast.${action}Failed`, { name: app.name }),
        description: message,
      });
    }
    await appsQuery.refresh();
  }

  function openBulk(action: BulkAction): void {
    setBulk({ action, targets: bulkTargets(apps, action) });
  }

  async function handleBulk(): Promise<void> {
    if (!bulk) {
      return;
    }
    setFailures([]);
    setBulkRunning(true);
    const failed: Failure[] = [];
    for (const app of bulk.targets) {
      const message = await runAction(app, bulk.action);
      if (message !== null) {
        failed.push({ name: app.name, message });
      }
    }
    setBulkRunning(false);
    setBulk(null);
    setFailures(failed);
    const done = bulk.targets.length - failed.length;
    if (failed.length === 0) {
      showFeedbackToast({ type: "success", title: t(`apps.bulk.${bulk.action}Done`, { count: done }) });
    } else {
      showFeedbackToast({
        type: "error",
        title: t(`apps.bulk.${bulk.action}Partial`, { done, total: bulk.targets.length }),
        description: failed.map((failure) => `${failure.name}: ${failure.message}`).join("\n"),
      });
    }
    await appsQuery.refresh();
  }

  let body: React.ReactElement | null;
  if (list === null) {
    body = appsQuery.error ? null : <LoadingBlock />;
  } else if (!list.available) {
    body = <DockerBanner message={list.message} checks={doctor?.checks ?? []} />;
  } else if (apps.length === 0) {
    body = (
      <EmptyState
        icon={Boxes}
        title={t("apps.installed.empty.title")}
        description={t("apps.installed.empty.description")}
        action={
          <Link to={CATALOG_PATH} className={buttonVariants()}>
            {t("apps.installed.empty.action")}
          </Link>
        }
      />
    );
  } else {
    body = (
      <ContainerView
        mode={view}
        apps={apps}
        updates={updates}
        busy={busy}
        locked={locked}
        onAction={(app, action) => void handleAction(app, action)}
        onLogs={setLogsApp}
      />
    );
  }

  const showControls = list !== null && list.available && apps.length > 0;

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold font-heading">{t("apps.installed.title")}</h1>
          <p className="text-muted-foreground">{t("apps.installed.description")}</p>
        </div>
        {showControls ? (
          <div className="flex flex-wrap items-center gap-2">
            <SegmentedChoice
              name={VIEW_FIELD}
              value={view}
              onChange={(value) => setChosenView(value === TABLE ? TABLE : CARDS)}
              options={[
                { value: CARDS, label: t("apps.view.cards") },
                { value: TABLE, label: t("apps.view.table") },
              ]}
            />
            <div role="group" aria-label={t("apps.bulk.label")} className="flex gap-2">
              <Button variant="outline" disabled={locked || startable.length === 0} onClick={() => openBulk(START)}>
                <Play aria-hidden="true" />
                {t("apps.bulk.start")}
              </Button>
              <Button variant="outline" disabled={locked || stoppable.length === 0} onClick={() => openBulk(STOP)}>
                <Square aria-hidden="true" />
                {t("apps.bulk.stop")}
              </Button>
            </div>
          </div>
        ) : null}
      </div>

      {appsQuery.error ? (
        <Banner
          tone="error"
          title={appsQuery.error}
          action={
            <Button size="xs" variant="outline" onClick={() => void appsQuery.refresh()}>
              {t("apps.installed.retry")}
            </Button>
          }
        />
      ) : null}
      {failures.length > 0 ? (
        <Banner
          tone="error"
          title={t("apps.errors.actionsFailed", { count: failures.length })}
          description={
            <ul className="list-disc ps-5">
              {failures.map((failure) => (
                <li key={failure.name}>
                  {failure.name}: {failure.message}
                </li>
              ))}
            </ul>
          }
          onDismiss={() => setFailures([])}
        />
      ) : null}
      {updatesQuery.error && list?.available ? <Banner tone="warning" title={updatesQuery.error} /> : null}

      {body}

      <ConfirmDialog
        open={bulk !== null}
        onOpenChange={(open) => {
          if (!open && !bulkRunning) {
            setBulk(null);
          }
        }}
        title={t(`apps.bulk.${bulk?.action ?? "start"}Title`, { count: bulk?.targets.length ?? 0 })}
        description={t(`apps.bulk.${bulk?.action ?? "start"}Description`)}
        items={bulk?.targets.map((app) => app.name)}
        confirmLabel={t(`apps.bulk.${bulk?.action ?? "start"}Confirm`)}
        destructive={bulk?.action === "stop"}
        loading={bulkRunning}
        onConfirm={() => void handleBulk()}
      />

      <LogsPanel app={logsApp} onClose={() => setLogsApp(null)} />
    </div>
  );
}
