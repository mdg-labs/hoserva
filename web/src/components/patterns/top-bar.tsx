import type React from "react";
import { Bell, ListChecks, LogOut, Moon, Sun } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { JobProgress } from "@/components/patterns/job-progress";
import { StatusBadge } from "@/components/patterns/status-badge";
import {
  arrayStatusLabel,
  arrayStatusTone,
  parityFreshnessLabel,
} from "@/components/patterns/system-status";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Button } from "@/components/ui/button";
import { Menu, MenuContent, MenuItem, MenuSeparator, MenuTrigger } from "@/components/ui/menu";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { useActiveJobs } from "@/hooks/use-active-jobs";
import { jobDetailPath, PATHS } from "@/hooks/paths";
import { useSystemData } from "@/hooks/use-system-status";
import { useAuth } from "@/lib/api/auth-context";
import { hoservaClient } from "@/lib/api/client";

export function TopBar(): React.ReactElement {
  const { t } = useTranslation();
  const { user } = useAuth();
  const { status, doctor, jobs } = useSystemData();
  const activeJobs = useActiveJobs(jobs);
  const arrayLabel = arrayStatusLabel(status, t);
  const arrayTone = arrayStatusTone(status);
  const parity = parityFreshnessLabel(status, doctor, t);
  const activeCount = status?.activeJobs ?? activeJobs.length;

  const handleLogout = async (): Promise<void> => {
    await hoservaClient.POST("/auth/logout");
    window.location.assign("/login");
  };

  const toggleTheme = (): void => {
    document.documentElement.classList.toggle("dark");
  };

  const initials = user?.username?.slice(0, 2).toUpperCase() ?? "?";

  return (
    <div className="flex flex-1 items-center justify-end gap-2">
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
      <Button size="icon-sm" variant="ghost" aria-label={t("topBar.notifications.ariaLabel")}>
        <Bell aria-hidden="true" />
      </Button>
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
