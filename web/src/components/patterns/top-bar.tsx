import type React from "react";
import { useEffect, useReducer, useState } from "react";
import { Bell, ListChecks, LogOut, Moon, Sun } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { JobProgress } from "@/components/patterns/job-progress";
import {
  notificationAlertIds,
  notificationInboxReducer,
} from "@/components/patterns/notification-inbox";
import { StatusBadge } from "@/components/patterns/status-badge";
import {
  arrayStatusLabel,
  arrayStatusTone,
  parityFreshnessLabel,
} from "@/components/patterns/system-status";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Menu, MenuContent, MenuItem, MenuSeparator, MenuTrigger } from "@/components/ui/menu";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Sheet,
  SheetHeader,
  SheetPanel,
  SheetPopup,
  SheetTitle,
} from "@/components/ui/sheet";
import { useActiveJobs } from "@/hooks/use-active-jobs";
import { useIsMobile } from "@/hooks/use-media-query";
import { jobDetailPath, PATHS } from "@/hooks/paths";
import { useSystemData } from "@/hooks/use-system-status";
import { useAuth } from "@/lib/api/auth-context";
import { getNotifications, postAuthLogout, postNotificationsReadAll } from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import {
  subscribeToEvents,
  type NotificationAlert,
  type NotificationGroup,
} from "@/lib/api/events";

type NotificationLevel = NotificationAlert["level"];

const notificationSheetSide = "bottom" as const;

function notificationTone(level: NotificationLevel): "info" | "warning" | "error" {
  switch (level) {
    case "critical":
    case "error":
      return "error";
    case "warning":
      return "warning";
    default:
      return "info";
  }
}

function NotificationInboxPanel({
  groups,
  unreadCount,
  onMarkAllRead,
  error,
}: {
  groups: NotificationGroup[];
  unreadCount: number;
  onMarkAllRead: () => void;
  error?: string | null;
}): React.ReactElement {
  const { t } = useTranslation();

  return (
    <div className="flex max-h-[min(24rem,70vh)] min-h-0 flex-col">
      <div className="flex items-center justify-between gap-2 border-b px-4 py-3">
        <h2 className="font-medium text-sm">{t("topBar.notifications.panelTitle")}</h2>
        {unreadCount > 0 ? (
          <Button size="xs" variant="ghost" onClick={onMarkAllRead}>
            {t("topBar.notifications.markAllRead")}
          </Button>
        ) : null}
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto p-2">
        {error ? (
          <p role="alert" className="text-destructive px-2 py-4 text-sm">{error}</p>
        ) : groups.length === 0 ? (
          <p className="text-muted-foreground px-2 py-4 text-sm">{t("topBar.notifications.empty")}</p>
        ) : (
          <div className="flex flex-col gap-4">
            {groups.map((group) => (
              <section key={group.eventType}>
                <h3 className="px-2 pb-1 font-medium text-muted-foreground text-xs uppercase tracking-wide">
                  {t(`topBar.notifications.eventTypes.${group.eventType}`)}
                </h3>
                <ul className="flex flex-col gap-2">
                  {group.alerts.map((alert) => (
                    <li
                      key={alert.id}
                      className={`rounded-lg border px-3 py-2 ${alert.read ? "opacity-70" : "bg-muted/30"}`}
                    >
                      <div className="mb-1 flex items-center gap-2">
                        <StatusBadge tone={notificationTone(alert.level)}>{alert.title}</StatusBadge>
                      </div>
                      <p className="text-muted-foreground text-sm">{alert.message}</p>
                    </li>
                  ))}
                </ul>
              </section>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

export function TopBar(): React.ReactElement {
  const { t } = useTranslation();
  const { user } = useAuth();
  const { status, doctor, jobs } = useSystemData();
  const activeJobs = useActiveJobs(jobs);
  const isMobile = useIsMobile();
  const arrayLabel = arrayStatusLabel(status, t);
  const arrayTone = arrayStatusTone(status);
  const parity = parityFreshnessLabel(status, doctor, t);
  const activeCount = status?.activeJobs ?? activeJobs.length;
  const [logoutError, setLogoutError] = useState<string | null>(null);
  const [notificationOpen, setNotificationOpen] = useState(false);
  const [inbox, dispatchInbox] = useReducer(notificationInboxReducer, {
    groups: [],
    unreadCount: 0,
  });
  const { groups: notificationGroups, unreadCount } = inbox;

  const notificationsQuery = useApiQuery({
    queryKey: "top-bar-notifications",
    queryFn: (signal) => getNotifications(signal),
  });
  const markReadMutation = useApiMutation({ mutationFn: postNotificationsReadAll });
  const logoutMutation = useApiMutation({
    mutationFn: postAuthLogout,
    fallbackError: t("topBar.userMenu.logoutFailed"),
  });

  useEffect(() => {
    if (notificationsQuery.data) {
      dispatchInbox({
        type: "snapshot",
        groups: notificationsQuery.data.groups,
        unreadCount: notificationsQuery.data.unreadCount,
      });
    }
  }, [notificationsQuery.data]);

  useEffect(() => {
    return subscribeToEvents((event) => {
      if (event.event !== "notification") {
        return;
      }
      dispatchInbox({
        type: "alert",
        alert: {
          id: event.data.id,
          eventType: event.data.eventType,
          level: event.data.level,
          title: event.data.title,
          message: event.data.message,
          createdAt: event.data.createdAt,
          read: false,
        },
      });
    });
  }, []);

  const handleMarkAllRead = async (): Promise<void> => {
    const knownIds = notificationAlertIds(notificationGroups);
    const result = await markReadMutation.mutate(undefined);
    if (result.ok && result.data) {
      dispatchInbox({ type: "markKnownRead", knownIds, unreadCount: result.data.unreadCount });
    }
  };

  const handleLogout = async (): Promise<void> => {
    const result = await logoutMutation.mutate(undefined);
    if (!result.ok) {
      if (!result.aborted) {
        setLogoutError(result.error || t("topBar.userMenu.logoutFailed"));
      }
      return;
    }
    window.location.assign("/login");
  };

  const toggleTheme = (): void => {
    document.documentElement.classList.toggle("dark");
  };

  const initials = user?.username?.slice(0, 2).toUpperCase() ?? "?";

  const notificationTrigger = (
    <Button size="icon-sm" variant="outline" aria-label={t("topBar.notifications.ariaLabel")}>
      <Bell aria-hidden="true" />
      {unreadCount > 0 ? (
        <Badge variant="destructive" size="sm" className="absolute -top-1 -right-1 min-w-4 px-1">
          {unreadCount > 99 ? "99+" : unreadCount}
        </Badge>
      ) : null}
    </Button>
  );

  const notificationPanel = (
    <NotificationInboxPanel
      groups={notificationGroups}
      unreadCount={unreadCount}
      onMarkAllRead={() => void handleMarkAllRead()}
      error={notificationsQuery.error}
    />
  );

  return (
    <div className="flex flex-1 items-center justify-end gap-2">
      {logoutError ? (
        <span role="alert" className="text-destructive text-sm">
          {logoutError}
        </span>
      ) : null}
      <Button size="sm" variant="outline" render={<Link to={PATHS.storage} />} className="hidden sm:inline-flex">
        <StatusBadge tone={arrayTone}>{arrayLabel}</StatusBadge>
      </Button>
      <StatusBadge tone={parity.tone}>{parity.label}</StatusBadge>
      <Popover>
        <PopoverTrigger
          render={
            <Button size="sm" variant="outline" aria-label={t("topBar.jobs.ariaLabel")}>
              <ListChecks aria-hidden="true" />
              <span>{t("topBar.jobs.count", { count: activeCount })}</span>
            </Button>
          }
        />
        <PopoverContent className="w-80">
          {activeJobs.length === 0 ? (
            <p className="text-muted-foreground text-sm">{t("topBar.jobs.empty")}</p>
          ) : (
            <div className="flex flex-col gap-3">
              {activeJobs.map((job) => (
                <Link key={job.id} to={jobDetailPath(job.id)} className="no-underline text-inherit">
                  <JobProgress job={job} />
                </Link>
              ))}
              <Button size="sm" variant="ghost" render={<Link to={PATHS.jobs} />}>
                {t("topBar.jobs.viewAll")}
              </Button>
            </div>
          )}
        </PopoverContent>
      </Popover>
      {isMobile ? (
        <Sheet onOpenChange={setNotificationOpen} open={notificationOpen}>
          <Button
            size="icon-sm"
            variant="outline"
            aria-label={t("topBar.notifications.ariaLabel")}
            className="relative"
            onClick={() => setNotificationOpen(true)}
          >
            <Bell aria-hidden="true" />
            {unreadCount > 0 ? (
              <Badge variant="destructive" size="sm" className="absolute -top-1 -right-1 min-w-4 px-1">
                {unreadCount > 99 ? "99+" : unreadCount}
              </Badge>
            ) : null}
          </Button>
          <SheetPopup side={notificationSheetSide} className="h-[min(80vh,28rem)]">
            <SheetHeader>
              <SheetTitle>{t("topBar.notifications.panelTitle")}</SheetTitle>
            </SheetHeader>
            <SheetPanel>{notificationPanel}</SheetPanel>
          </SheetPopup>
        </Sheet>
      ) : (
        <Popover>
          <PopoverTrigger render={<span className="relative inline-flex">{notificationTrigger}</span>} />
          <PopoverContent className="w-96 p-0">{notificationPanel}</PopoverContent>
        </Popover>
      )}
      <Menu>
        <MenuTrigger
          render={
            <Button size="icon-sm" variant="ghost" aria-label={t("topBar.userMenu.ariaLabel")}>
              <Avatar>
                <AvatarFallback>{initials}</AvatarFallback>
              </Avatar>
            </Button>
          }
        />
        <MenuContent>
          <MenuItem disabled>{user?.username}</MenuItem>
          <MenuSeparator />
          <MenuItem onClick={toggleTheme}>
            <Sun className="dark:hidden" aria-hidden="true" />
            <Moon className="hidden dark:inline" aria-hidden="true" />
            {t("topBar.userMenu.toggleTheme")}
          </MenuItem>
          <MenuItem onClick={() => void handleLogout()}>
            <LogOut aria-hidden="true" />
            {t("topBar.userMenu.logout")}
          </MenuItem>
        </MenuContent>
      </Menu>
    </div>
  );
}
