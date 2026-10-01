import { MoreHorizontal, Play, ScrollText, Square } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Link, useNavigate } from "react-router-dom";

import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardAction, CardDescription, CardFooter, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Menu, MenuContent, MenuItem, MenuTrigger } from "@/components/ui/menu";
import {
  appDetailPath,
  RESTART,
  START,
  STOP,
  canStart,
  canStop,
  publishedPorts,
  stateTone,
  updateSummary,
  type App,
  type AppUpdate,
  type LifecycleAction,
} from "@/routes/apps/containers";

export interface ContainerViewProps {
  apps: App[];
  // null while the update check is loading or has failed: no row says
  // anything about updates then.
  updates: Map<string, AppUpdate> | null;
  busy: Record<string, LifecycleAction>;
  locked: boolean;
  onAction: (app: App, action: LifecycleAction) => void;
  onLogs: (app: App) => void;
}

function imageRef(app: App): string {
  return app.tag ? `${app.image}:${app.tag}` : app.image;
}

function StateBadges({ app }: { app: App }): React.ReactElement {
  const { t } = useTranslation();
  const running = app.state === "running";
  return (
    <div className="flex flex-wrap gap-1.5">
      <StatusBadge tone={stateTone(app.state)}>{t(`apps.state.${app.state}`)}</StatusBadge>
      {running && app.health === "unhealthy" ? (
        <StatusBadge tone="error">{t("apps.health.unhealthy")}</StatusBadge>
      ) : null}
      {running && app.health === "starting" ? (
        <StatusBadge tone="info">{t("apps.health.starting")}</StatusBadge>
      ) : null}
    </div>
  );
}

function UpdateInfo({ app, updates }: { app: App; updates: Map<string, AppUpdate> | null }): React.ReactElement | null {
  const { t } = useTranslation();
  if (updates === null) {
    return null;
  }
  const summary = updateSummary(updates.get(app.name));
  if (summary === null) {
    return null;
  }
  if (summary.kind === "available") {
    let label = t("apps.update.available");
    if (summary.change === "new_build") {
      label = t("apps.update.newBuild");
    } else if (summary.change === "new_version" && summary.tag) {
      label = t("apps.update.newVersion", { tag: summary.tag });
    }
    return (
      <div>
        <StatusBadge tone="info">{label}</StatusBadge>
      </div>
    );
  }
  return (
    <div className="flex flex-col items-start gap-1">
      <StatusBadge tone="outline">{t("apps.update.notChecked")}</StatusBadge>
      {summary.message ? <span className="text-muted-foreground text-xs">{summary.message}</span> : null}
    </div>
  );
}

function PortLinks({ app }: { app: App }): React.ReactElement {
  const { t } = useTranslation();
  const ports = publishedPorts(app.ports, window.location.hostname);
  if (ports.length === 0) {
    return <span className="text-muted-foreground text-sm">{t("apps.ports.none")}</span>;
  }
  return (
    <div className="flex flex-wrap gap-1.5">
      {ports.map((port) =>
        port.href ? (
          <Badge
            key={port.key}
            variant="outline"
            size="lg"
            render={
              <a
                href={port.href}
                target="_blank"
                rel="noopener noreferrer"
                aria-label={t("apps.ports.open", { name: app.name, port: port.port })}
              />
            }
          >
            {port.port}
          </Badge>
        ) : (
          <Badge key={port.key} variant="outline" size="lg">
            {port.port}/{port.protocol}
          </Badge>
        ),
      )}
    </div>
  );
}

function PrimaryActions({
  app,
  busy,
  locked,
  onAction,
  onLogs,
}: {
  app: App;
  busy: LifecycleAction | undefined;
  locked: boolean;
  onAction: ContainerViewProps["onAction"];
  onLogs: ContainerViewProps["onLogs"];
}): React.ReactElement {
  const { t } = useTranslation();
  const disabled = locked || busy !== undefined;
  return (
    <div className="flex flex-wrap items-center gap-2">
      {canStart(app.state) ? (
        <Button
          variant="outline"
          disabled={disabled}
          loading={busy === START}
          aria-label={t("apps.actions.startNamed", { name: app.name })}
          onClick={() => onAction(app, START)}
        >
          <Play aria-hidden="true" />
          {t("apps.actions.start")}
        </Button>
      ) : null}
      {canStop(app.state) ? (
        <Button
          variant="outline"
          disabled={disabled}
          loading={busy === STOP}
          aria-label={t("apps.actions.stopNamed", { name: app.name })}
          onClick={() => onAction(app, STOP)}
        >
          <Square aria-hidden="true" />
          {t("apps.actions.stop")}
        </Button>
      ) : null}
      <Button
        variant="outline"
        aria-label={t("apps.actions.logsNamed", { name: app.name })}
        onClick={() => onLogs(app)}
      >
        <ScrollText aria-hidden="true" />
        {t("apps.actions.logs")}
      </Button>
    </div>
  );
}

function RowMenu({
  app,
  busy,
  locked,
  onAction,
}: {
  app: App;
  busy: LifecycleAction | undefined;
  locked: boolean;
  onAction: ContainerViewProps["onAction"];
}): React.ReactElement {
  const { t } = useTranslation();
  const navigate = useNavigate();
  return (
    <Menu>
      <MenuTrigger
        render={
          <Button size="icon" variant="ghost" aria-label={t("apps.actions.menu", { name: app.name })}>
            <MoreHorizontal aria-hidden="true" />
          </Button>
        }
      />
      <MenuContent>
        {canStop(app.state) ? (
          <MenuItem disabled={locked || busy !== undefined} onClick={() => onAction(app, RESTART)}>
            {t("apps.actions.restart")}
          </MenuItem>
        ) : null}
        <MenuItem onClick={() => navigate(appDetailPath(app.name))}>{t("apps.actions.details")}</MenuItem>
      </MenuContent>
    </Menu>
  );
}

function ContainerCards({ apps, updates, busy, locked, onAction, onLogs }: ContainerViewProps): React.ReactElement {
  return (
    <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
      {apps.map((app) => (
        <Card key={app.id}>
          <CardHeader>
            <CardTitle className="break-all">
              <Link to={appDetailPath(app.name)}>{app.name}</Link>
            </CardTitle>
            <CardDescription className="break-all">{imageRef(app)}</CardDescription>
            <CardAction>
              <RowMenu app={app} busy={busy[app.id]} locked={locked} onAction={onAction} />
            </CardAction>
          </CardHeader>
          <CardPanel className="flex flex-col gap-3">
            <StateBadges app={app} />
            <p className="text-muted-foreground text-sm">{app.status}</p>
            <PortLinks app={app} />
            <UpdateInfo app={app} updates={updates} />
          </CardPanel>
          <CardFooter>
            <PrimaryActions app={app} busy={busy[app.id]} locked={locked} onAction={onAction} onLogs={onLogs} />
          </CardFooter>
        </Card>
      ))}
    </div>
  );
}

function ContainerTable({ apps, updates, busy, locked, onAction, onLogs }: ContainerViewProps): React.ReactElement {
  const { t } = useTranslation();
  const columns: DataTableColumn<App>[] = [
    {
      id: "name",
      header: t("apps.columns.name"),
      cell: (app) => (
        <Link to={appDetailPath(app.name)} className="font-medium">
          {app.name}
        </Link>
      ),
    },
    { id: "image", header: t("apps.columns.image"), cell: (app) => <span className="break-all">{imageRef(app)}</span> },
    {
      id: "state",
      header: t("apps.columns.state"),
      cell: (app) => (
        <div className="flex flex-col items-start gap-1">
          <StateBadges app={app} />
          <span className="text-muted-foreground text-xs">{app.status}</span>
        </div>
      ),
    },
    { id: "ports", header: t("apps.columns.ports"), cell: (app) => <PortLinks app={app} /> },
    { id: "update", header: t("apps.columns.update"), cell: (app) => <UpdateInfo app={app} updates={updates} /> },
    {
      id: "actions",
      header: t("apps.columns.actions"),
      cell: (app) => (
        <div className="flex items-center gap-1">
          <PrimaryActions app={app} busy={busy[app.id]} locked={locked} onAction={onAction} onLogs={onLogs} />
          <RowMenu app={app} busy={busy[app.id]} locked={locked} onAction={onAction} />
        </div>
      ),
    },
  ];
  return <DataTable columns={columns} rows={apps} getRowKey={(app) => app.id} />;
}

export function ContainerView({ mode, ...props }: ContainerViewProps & { mode: "cards" | "table" }): React.ReactElement {
  return mode === "cards" ? <ContainerCards {...props} /> : <ContainerTable {...props} />;
}
