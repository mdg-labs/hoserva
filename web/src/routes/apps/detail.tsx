import { ArrowLeft, Boxes, Play, RefreshCw, RotateCw, Square, Trash2 } from "lucide-react";
import type { TFunction } from "i18next";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useNavigate, useParams } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { DetailTabs, type DetailTab } from "@/components/patterns/detail-tabs";
import { EmptyState } from "@/components/patterns/empty-state";
import { showFeedbackToast } from "@/components/patterns/feedback-toast";
import { FormOverlay } from "@/components/patterns/form-overlay";
import { LoadingBlock } from "@/components/patterns/loading";
import { TypedConfirm } from "@/components/patterns/typed-confirm";
import { Button, buttonVariants } from "@/components/ui/button";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Tooltip, TooltipPopup, TooltipTrigger } from "@/components/ui/tooltip";
import { jobDetailPath } from "@/hooks/paths";
import { useSystemData } from "@/hooks/use-system-status";
import type { components } from "@/lib/api/client";
import type { ApiError } from "@/lib/api/errors";
import {
  deleteApp,
  deleteStack,
  getApp,
  getApps,
  postAppRecreate,
  postAppRestart,
  postAppStart,
  postAppStop,
} from "@/lib/api/operations";
import { apiErrorMessage, isAbortError, type ClientResult } from "@/lib/api/request";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import { PortLinks, StateBadges } from "@/routes/apps/container-view";
import {
  appComposePath,
  canRemove,
  canStart,
  canStop,
  durationParts,
  RESTART,
  START,
  STOP,
  removeRefusalKey,
  type App,
  type AppMountLocation,
  type LifecycleAction,
} from "@/routes/apps/containers";
import { DockerBanner } from "@/routes/apps/docker-banner";
import { LogsTab } from "@/routes/apps/logs-tab";
import { StatsTab } from "@/routes/apps/stats-tab";
import { UpdateTab } from "@/routes/apps/update-tab";

type Job = components["schemas"]["Job"];
type ListAppsOK = components["schemas"]["ListAppsOK"];

const POLL_MS = 15_000;
const TAB_OVERVIEW = "overview";
const TAB_LOGS = "logs";
const TAB_STATS = "stats";
const TAB_CONFIG = "config";
const TAB_UPDATE = "update";
const RECREATE = "recreate" as const;
const APPS_PATH = "/apps";
const CLOCK_MS = 30_000;

// What opening a container's page can find: the container, no container by
// that name, or no Docker to ask. The last two are answers, not failures.
type Detail =
  | { kind: "app"; app: App }
  | { kind: "missing" }
  | { kind: "unavailable"; message: string };

const NOT_FOUND_CODE = "app_not_found";
const DOCKER_UNAVAILABLE_CODE = "docker_unavailable";

async function loadDetail(name: string, signal: AbortSignal): Promise<ClientResult<Detail>> {
  const result = await getApp(name, signal);
  if (result.error?.code === NOT_FOUND_CODE) {
    return { data: { kind: "missing" }, response: { ok: true } };
  }
  if (result.error?.code === DOCKER_UNAVAILABLE_CODE) {
    return { data: { kind: "unavailable", message: result.error.message }, response: { ok: true } };
  }
  if (result.error !== undefined || result.response?.ok === false || result.data === undefined) {
    return { error: result.error, response: { ok: false } };
  }
  return { data: { kind: "app", app: result.data }, response: { ok: true } };
}

type RemoveFailure = { code?: string; message: string };

function failureOf(result: { error?: ApiError }): RemoveFailure {
  return { code: result.error?.code, message: apiErrorMessage(result.error) };
}

function formatDuration(ms: number, t: TFunction): string {
  const parts = durationParts(ms);
  if (parts.length === 0) {
    return t("apps.detail.duration.lessThanMinute");
  }
  return parts.map((part) => t(`apps.detail.duration.${part.unit}`, { count: part.count })).join(" ");
}

function locationLabel(location: AppMountLocation | undefined, t: TFunction): string {
  if (location === undefined) {
    return t("apps.detail.location.unknown");
  }
  switch (location.kind) {
    case "pool":
      return location.share
        ? t("apps.detail.location.poolShare", { share: location.share })
        : t("apps.detail.location.pool");
    case "disk":
      return t("apps.detail.location.disk", { number: location.disk });
    case "cache":
      return t("apps.detail.location.cache");
    default:
      return t("apps.detail.location.outside");
  }
}

function Fact({ label, children }: { label: string; children: React.ReactNode }): React.ReactElement {
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <dt className="text-muted-foreground text-xs">{label}</dt>
      <dd className="min-w-0 break-words text-sm">{children}</dd>
    </div>
  );
}

function healthLabel(app: App, t: TFunction): string {
  return t(`apps.health.${app.health}`);
}

// The page's own clock, so an uptime is computed from a value React was given
// rather than read during render.
function useNow(intervalMs: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs]);
  return now;
}

function Overview({ app }: { app: App }): React.ReactElement {
  const now = useNow(CLOCK_MS);
  const { t } = useTranslation();
  const running = app.state === "running";
  const startedAt = app.startedAt ? new Date(app.startedAt).getTime() : null;
  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader>
          <CardTitle>{t("apps.detail.overview.title")}</CardTitle>
        </CardHeader>
        <CardPanel>
          <dl className="grid gap-4 sm:grid-cols-2">
            <Fact label={t("apps.detail.overview.state")}>
              <StateBadges app={app} />
            </Fact>
            <Fact label={t("apps.detail.overview.health")}>{healthLabel(app, t)}</Fact>
            <Fact label={t("apps.detail.overview.image")}>
              <span className="break-all">{app.image}</span>
            </Fact>
            <Fact label={t("apps.detail.overview.tag")}>{app.tag || t("apps.detail.notKnown")}</Fact>
            <Fact label={t("apps.detail.overview.created")}>
              {app.createdAt ? new Date(app.createdAt).toLocaleString() : t("apps.detail.notKnown")}
            </Fact>
            <Fact label={t("apps.detail.overview.uptime")}>
              {!running
                ? t("apps.detail.overview.notRunning")
                : startedAt === null
                  ? t("apps.detail.notKnown")
                  : formatDuration(Math.max(0, now - startedAt), t)}
            </Fact>
            <Fact label={t("apps.detail.overview.restarts")}>
              {app.restartCount === undefined ? t("apps.detail.notKnown") : app.restartCount}
            </Fact>
            <Fact label={t("apps.detail.overview.ports")}>
              <PortLinks app={app} />
            </Fact>
          </dl>
        </CardPanel>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("apps.detail.mounts.title")}</CardTitle>
        </CardHeader>
        <CardPanel>
          {app.mounts.length === 0 ? (
            <p className="text-muted-foreground text-sm">{t("apps.detail.mounts.none")}</p>
          ) : (
            <ul className="flex flex-col gap-4">
              {app.mounts.map((mount) => (
                <li key={mount.destination} className="flex min-w-0 flex-col gap-1 text-sm">
                  <span className="break-all font-medium">
                    {t("apps.detail.mounts.path", {
                      source: mount.source ?? t("apps.detail.mounts.noSource"),
                      destination: mount.destination,
                    })}
                  </span>
                  <span className="text-muted-foreground">
                    {mount.source === undefined ? t("apps.detail.notKnown") : locationLabel(mount.location, t)}
                    {" · "}
                    {mount.readWrite ? t("apps.detail.mounts.readWrite") : t("apps.detail.mounts.readOnly")}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </CardPanel>
      </Card>

      {app.stack !== undefined ? (
        <Card>
          <CardHeader>
            <CardTitle>{t("apps.detail.stack.title")}</CardTitle>
          </CardHeader>
          <CardPanel className="flex flex-col items-start gap-3">
            <p className="text-sm">{t("apps.detail.stack.description", { stack: app.stack })}</p>
            <Link to={appComposePath(app.name)} className={buttonVariants({ variant: "outline" })}>
              {t("apps.detail.stack.edit")}
            </Link>
          </CardPanel>
        </Card>
      ) : null}
    </div>
  );
}

function ActionButton({
  label,
  icon,
  reason,
  loading = false,
  disabled = false,
  destructive = false,
  onClick,
}: {
  label: string;
  icon: React.ReactNode;
  // Why the action cannot be used right now; null means it can.
  reason: string | null;
  loading?: boolean;
  disabled?: boolean;
  destructive?: boolean;
  onClick: () => void;
}): React.ReactElement {
  const variant = destructive ? "destructive-outline" : "outline";
  if (reason !== null) {
    return (
      <Tooltip>
        <TooltipTrigger render={<span tabIndex={0} className="inline-flex rounded-lg outline-none focus-visible:ring-2 focus-visible:ring-ring" />}>
          <Button variant={variant} disabled tabIndex={-1}>
            {icon}
            {label}
          </Button>
        </TooltipTrigger>
        <TooltipPopup>{reason}</TooltipPopup>
      </Tooltip>
    );
  }
  return (
    <Button variant={variant} disabled={disabled} loading={loading} onClick={onClick}>
      {icon}
      {label}
    </Button>
  );
}

// The dialog exists only while the removal is being asked about, so each
// opening starts with the appdata choice off and nothing typed.
function RemoveDialog({ app, onClose }: { app: App; onClose: () => void }): React.ReactElement {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const stack = app.stack;
  const [deleteAppdata, setDeleteAppdata] = useState(false);
  const [typed, setTyped] = useState("");
  const [removing, setRemoving] = useState(false);
  const [failure, setFailure] = useState<RemoveFailure | null>(null);

  const membersQuery = useApiQuery<ListAppsOK>({
    queryKey: ["apps", "stack-members", stack],
    queryFn: (signal) => getApps(signal),
    enabled: stack !== undefined,
    fallbackError: t("apps.detail.remove.membersFailed"),
  });

  const members = useMemo(() => {
    if (stack === undefined) {
      return [app.name];
    }
    const list = membersQuery.data;
    if (list === null || !list.available) {
      return null;
    }
    return list.apps.filter((other) => other.stack === stack).map((other) => other.name);
  }, [app.name, stack, membersQuery.data]);

  const phrase = stack ?? app.name;
  const confirmed = !deleteAppdata || typed === phrase;
  const ready = members !== null && members.length > 0 && confirmed;

  async function handleRemove(): Promise<void> {
    setRemoving(true);
    setFailure(null);
    try {
      const result =
        stack === undefined ? await deleteApp(app.id, deleteAppdata) : await deleteStack(stack, deleteAppdata);
      if (result.error !== undefined || result.response?.ok === false) {
        setFailure(failureOf(result));
        return;
      }
      showFeedbackToast({ type: "success", title: t("apps.detail.remove.done", { name: stack ?? app.name }) });
      navigate(APPS_PATH);
    } catch (err: unknown) {
      if (!isAbortError(err)) {
        setFailure({ message: err instanceof Error ? err.message : t("apps.detail.remove.failed") });
      }
    } finally {
      setRemoving(false);
    }
  }

  const refusalKey = removeRefusalKey(failure?.code);
  let membersProblem: string | null = null;
  if (stack !== undefined) {
    if (membersQuery.error) {
      membersProblem = membersQuery.error;
    } else if (membersQuery.data !== null && !membersQuery.data.available) {
      membersProblem = membersQuery.data.message ?? t("apps.detail.remove.membersFailed");
    }
  }

  return (
    <FormOverlay
      open
      onOpenChange={(next) => {
        if (!removing && !next) {
          onClose();
        }
      }}
      title={stack === undefined ? t("apps.detail.remove.title", { name: app.name }) : t("apps.detail.remove.stackTitle", { stack })}
      description={stack === undefined ? t("apps.detail.remove.description") : t("apps.detail.remove.stackDescription", { stack })}
      footer={
        <>
          <Button variant="outline" disabled={removing} onClick={onClose}>
            {t("confirm.cancel")}
          </Button>
          <Button variant="destructive" disabled={!ready || removing} loading={removing} onClick={() => void handleRemove()}>
            {t("apps.detail.remove.confirm")}
          </Button>
        </>
      }
    >
      {failure ? (
        <Banner
          tone="error"
          title={refusalKey ? t(`apps.detail.remove.refusals.${refusalKey}`) : t("apps.detail.remove.failed")}
          description={failure.message}
        />
      ) : null}
      {membersProblem ? (
        <Banner
          tone="error"
          title={membersProblem}
          action={
            <Button size="xs" variant="outline" onClick={() => void membersQuery.refresh()}>
              {t("apps.installed.retry")}
            </Button>
          }
        />
      ) : null}
      {members === null && membersProblem === null ? <LoadingBlock rows={1} /> : null}
      {members !== null ? (
        <div className="flex flex-col gap-2">
          <p className="text-sm font-medium">{t("apps.detail.remove.willRemove")}</p>
          <ul className="list-disc space-y-1 ps-5 text-sm">
            {members.map((name) => (
              <li key={name}>{name}</li>
            ))}
          </ul>
        </div>
      ) : null}
      <Field className="flex-row items-start gap-2">
        <Checkbox
          checked={deleteAppdata}
          disabled={removing}
          onCheckedChange={(checked) => setDeleteAppdata(checked === true)}
          aria-label={t("apps.detail.remove.appdata")}
        />
        <div className="flex min-w-0 flex-col gap-1">
          <FieldLabel className="cursor-default">{t("apps.detail.remove.appdata")}</FieldLabel>
          <FieldDescription>{t("apps.detail.remove.appdataDescription")}</FieldDescription>
        </div>
      </Field>
      {deleteAppdata ? (
        <TypedConfirm
          phrase={phrase}
          value={typed}
          onChange={setTyped}
          title={t("apps.detail.remove.typedTitle")}
          description={t("apps.detail.remove.typedDescription")}
          items={[t("apps.detail.remove.typedItem", { name: phrase })]}
        />
      ) : null}
    </FormOverlay>
  );
}

export function AppDetailPage(): React.ReactElement {
  const { t } = useTranslation();
  const { name = "" } = useParams();
  const { doctor } = useSystemData();
  const [busy, setBusy] = useState<LifecycleAction | typeof RECREATE | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [recreateJob, setRecreateJob] = useState<Job | null>(null);
  const [removeTarget, setRemoveTarget] = useState<App | null>(null);

  const query = useApiQuery<Detail>({
    queryKey: ["app", name],
    queryFn: (signal) => loadDetail(name, signal),
    pollIntervalMs: POLL_MS,
    fallbackError: t("apps.detail.loadFailed"),
  });
  const lifecycle = useApiMutation<{ id: string; action: LifecycleAction }, App>({
    mutationFn: ({ id, action }) => {
      if (action === "start") return postAppStart(id);
      if (action === "stop") return postAppStop(id);
      return postAppRestart(id);
    },
    fallbackError: t("apps.errors.actionFailed"),
  });
  const recreate = useApiMutation<string, Job>({
    mutationFn: (id) => postAppRecreate(id),
    fallbackError: t("apps.errors.actionFailed"),
  });

  const detail = query.data;
  const app = detail?.kind === "app" ? detail.app : null;

  async function handleAction(action: LifecycleAction): Promise<void> {
    if (app === null) {
      return;
    }
    setBusy(action);
    setActionError(null);
    setRecreateJob(null);
    try {
      const result = await lifecycle.mutate({ id: app.id, action });
      if (result.ok) {
        showFeedbackToast({ type: "success", title: t(`apps.toast.${action}Done`, { name: app.name }) });
      } else if (!result.aborted) {
        setActionError(result.error);
        showFeedbackToast({
          type: "error",
          title: t(`apps.toast.${action}Failed`, { name: app.name }),
          description: result.error,
        });
      }
    } finally {
      setBusy(null);
    }
    await query.refresh();
  }

  async function handleRecreate(): Promise<void> {
    if (app === null) {
      return;
    }
    setBusy(RECREATE);
    setActionError(null);
    setRecreateJob(null);
    try {
      const result = await recreate.mutate(app.id);
      if (result.ok && result.data !== undefined) {
        setRecreateJob(result.data);
      } else if (result.ok) {
        setActionError(t("apps.errors.actionFailed"));
      } else if (!result.aborted) {
        setActionError(result.error);
        showFeedbackToast({
          type: "error",
          title: t("apps.toast.recreateFailed", { name: app.name }),
          description: result.error,
        });
      }
    } finally {
      setBusy(null);
    }
  }

  const backLink = (
    <Link to={APPS_PATH} className={buttonVariants({ variant: "ghost", size: "sm" })}>
      <ArrowLeft aria-hidden="true" />
      {t("apps.detail.back")}
    </Link>
  );

  // The dialog sits outside the page body so it survives the page changing
  // underneath it: a removal that deletes the container but fails on its data
  // leaves the next load answering "not found", and the error must stay.
  const removeDialog = removeTarget ? <RemoveDialog app={removeTarget} onClose={() => setRemoveTarget(null)} /> : null;

  const body = ((): React.ReactElement => {
    if (detail === null) {
      return (
        <div className="flex flex-col gap-4">
          {backLink}
          {query.error ? (
            <Banner
              tone="error"
              title={query.error}
              action={
                <Button size="xs" variant="outline" onClick={() => void query.refresh()}>
                  {t("apps.installed.retry")}
                </Button>
              }
            />
          ) : (
            <LoadingBlock />
          )}
        </div>
      );
    }
    if (detail.kind === "missing") {
      return (
        <div className="flex flex-col gap-4">
          {backLink}
          <EmptyState
            icon={Boxes}
            title={t("apps.detail.notFound.title")}
            description={t("apps.detail.notFound.description", { name })}
            action={
              <Link to={APPS_PATH} className={buttonVariants()}>
                {t("apps.detail.notFound.action")}
              </Link>
            }
          />
        </div>
      );
    }
    if (detail.kind === "unavailable") {
      return (
        <div className="flex flex-col gap-4">
          {backLink}
          <DockerBanner message={detail.message} checks={doctor?.checks ?? []} />
        </div>
      );
    }

    const current = detail.app;
    const locked = busy !== null;
    const running = canStop(current.state);
    const removable = current.stack !== undefined || canRemove(current.state);
    const placeholder = <p className="text-muted-foreground text-sm">{t("apps.detail.placeholder")}</p>;
    const tabs: DetailTab[] = [
      { id: TAB_OVERVIEW, label: t("apps.detail.tabs.overview"), content: <Overview app={current} /> },
      { id: TAB_LOGS, label: t("apps.detail.tabs.logs"), content: <LogsTab app={current} /> },
      { id: TAB_STATS, label: t("apps.detail.tabs.stats"), content: <StatsTab app={current} /> },
      { id: TAB_CONFIG, label: t("apps.detail.tabs.config"), content: placeholder },
      {
        id: TAB_UPDATE,
        label: t("apps.detail.tabs.update"),
        content: <UpdateTab app={current} onChanged={() => void query.refresh()} />,
      },
    ];

    return (
      <div className="flex flex-col gap-4">
        {backLink}
        <div className="flex flex-col gap-3">
          <div className="min-w-0">
            <h1 className="break-all text-2xl font-semibold font-heading">{current.name}</h1>
            <p className="break-all text-muted-foreground">
              {current.tag ? `${current.image}:${current.tag}` : current.image}
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <StateBadges app={current} />
          </div>
          <div role="group" aria-label={t("apps.detail.actions.label")} className="flex flex-wrap gap-2">
            <ActionButton
              label={t("apps.actions.start")}
              icon={<Play aria-hidden="true" />}
              reason={canStart(current.state) ? null : t("apps.detail.actions.cannotStart")}
              disabled={locked}
              loading={busy === START}
              onClick={() => void handleAction(START)}
            />
            <ActionButton
              label={t("apps.actions.stop")}
              icon={<Square aria-hidden="true" />}
              reason={running ? null : t("apps.detail.actions.cannotStop")}
              disabled={locked}
              loading={busy === STOP}
              onClick={() => void handleAction(STOP)}
            />
            <ActionButton
              label={t("apps.actions.restart")}
              icon={<RotateCw aria-hidden="true" />}
              reason={running ? null : t("apps.detail.actions.cannotRestart")}
              disabled={locked}
              loading={busy === RESTART}
              onClick={() => void handleAction(RESTART)}
            />
            <ActionButton
              label={t("apps.actions.recreate")}
              icon={<RefreshCw aria-hidden="true" />}
              reason={current.state === "removing" ? t("apps.detail.actions.cannotRecreate") : null}
              disabled={locked}
              loading={busy === RECREATE}
              onClick={() => void handleRecreate()}
            />
            <ActionButton
              label={t("apps.actions.remove")}
              icon={<Trash2 aria-hidden="true" />}
              reason={removable ? null : t("apps.detail.actions.cannotRemove")}
              disabled={locked}
              destructive
              onClick={() => setRemoveTarget(current)}
            />
          </div>
        </div>

        {query.error ? (
          <Banner
            tone="error"
            title={query.error}
            action={
              <Button size="xs" variant="outline" onClick={() => void query.refresh()}>
                {t("apps.installed.retry")}
              </Button>
            }
          />
        ) : null}
        {actionError ? <Banner tone="error" title={actionError} onDismiss={() => setActionError(null)} /> : null}
        {recreateJob ? (
          <Banner
            tone="info"
            title={t("apps.detail.recreate.queued", { name: current.name })}
            description={t("apps.detail.recreate.description")}
            action={
              <Link to={jobDetailPath(recreateJob.id)} className={buttonVariants({ size: "xs", variant: "outline" })}>
                {t("apps.detail.recreate.viewJob")}
              </Link>
            }
            onDismiss={() => setRecreateJob(null)}
          />
        ) : null}

        <div className="[&_[data-slot=tabs-list]]:flex-nowrap [&_[data-slot=tabs-list]]:overflow-x-auto [&_[data-slot=tabs-tab]]:shrink-0 [&_[data-slot=tabs-tab]]:whitespace-nowrap">
          <DetailTabs tabs={tabs} />
        </div>


      </div>
    );
  })();

  return (
    <>
      {body}
      {removeDialog}
    </>
  );
}
