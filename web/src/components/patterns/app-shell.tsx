// `app-shell` (doc 03 "Shared patterns"): Sidebar, SidebarInset,
// SidebarTrigger inside SidebarProvider — the nine sidebar sections (doc 03
// "Navigation structure"); collapse and mobile behaviour come from
// SidebarProvider itself. Onboarding and login are not sidebar items.
import {
  FolderOpen,
  HardDrive,
  LayoutDashboard,
  LayoutGrid,
  ListChecks,
  MonitorSmartphone,
  Settings,
  Users,
  Wrench,
  type LucideIcon,
} from "lucide-react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link, useLocation } from "react-router-dom";

import {
  Sidebar,
  SidebarContent,
  SidebarGroup,
  SidebarGroupContent,
  SidebarHeader,
  SidebarInset,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  SidebarRail,
  SidebarTrigger,
} from "@/components/ui/sidebar";

interface NavEntry {
  to: string;
  labelKey: string;
  icon: LucideIcon;
}

const NAV_ENTRIES: NavEntry[] = [
  { to: "/", labelKey: "nav.dashboard", icon: LayoutDashboard },
  { to: "/storage", labelKey: "nav.storage", icon: HardDrive },
  { to: "/shares", labelKey: "nav.shares", icon: FolderOpen },
  { to: "/apps", labelKey: "nav.apps", icon: LayoutGrid },
  { to: "/vms", labelKey: "nav.vms", icon: MonitorSmartphone },
  { to: "/jobs", labelKey: "nav.jobs", icon: ListChecks },
  { to: "/users", labelKey: "nav.users", icon: Users },
  { to: "/settings", labelKey: "nav.settings", icon: Settings },
  { to: "/tools", labelKey: "nav.tools", icon: Wrench },
];

function isNavEntryActive(pathname: string, to: string): boolean {
  if (to === "/") {
    return pathname === "/";
  }
  return pathname === to || pathname.startsWith(`${to}/`);
}

export function AppShell({ children }: { children: ReactNode }): React.ReactElement {
  const { t } = useTranslation();
  const location = useLocation();

  return (
    <SidebarProvider>
      <Sidebar>
        <SidebarHeader>
          <span className="px-2 text-lg font-semibold font-heading">{t("shell.title")}</span>
        </SidebarHeader>
        <SidebarContent>
          <SidebarGroup>
            <SidebarGroupContent>
              <SidebarMenu>
                {NAV_ENTRIES.map(({ to, labelKey, icon: Icon }) => (
                  <SidebarMenuItem key={to}>
                    <SidebarMenuButton
                      isActive={isNavEntryActive(location.pathname, to)}
                      render={<Link to={to} />}
                    >
                      <Icon aria-hidden="true" />
                      <span>{t(labelKey)}</span>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                ))}
              </SidebarMenu>
            </SidebarGroupContent>
          </SidebarGroup>
        </SidebarContent>
        <SidebarRail />
      </Sidebar>
      <SidebarInset>
        <header className="flex h-14 shrink-0 items-center gap-2 border-b px-4">
          <SidebarTrigger aria-label={t("shell.toggleSidebar")} />
        </header>
        <main className="flex-1 p-4">{children}</main>
      </SidebarInset>
    </SidebarProvider>
  );
}
