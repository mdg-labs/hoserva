import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { createBrowserRouter, Route, RouterProvider, Routes } from "react-router-dom";

import { AppShell } from "@/components/patterns/app-shell";
import { AuthGate, AuthProvider, MinimalAuthLayout } from "@/lib/api/auth-guard";
import { watchSystemTheme } from "@/lib/theme";
import { DashboardPage } from "@/routes/dashboard";
import { JobDetailPage } from "@/routes/jobs/detail";
import { JobsPage } from "@/routes/jobs/index";
import { AppsPage } from "@/routes/apps/index";
import { CatalogPage } from "@/routes/apps/catalog";
import { CatalogDetailPage } from "@/routes/apps/catalog-detail";
import { ComposePage } from "@/routes/apps/compose";
import { InstallPage } from "@/routes/apps/install";
import { AppDetailPage } from "@/routes/apps/detail";
import { LoginPage } from "@/routes/login";
import { PlaceholderPage } from "@/routes/placeholder-page";
import { SectionLayout } from "@/routes/section-layout";
import { ShareDetailPage } from "@/routes/shares/detail";
import { SharesPage } from "@/routes/shares/index";
import { DiskDetailPage } from "@/routes/storage/disk-detail";
import { DisksPage } from "@/routes/storage/disks";
import { CachePage } from "@/routes/storage/cache";
import { ParityPage } from "@/routes/storage/parity";
import { PoolOverviewPage } from "@/routes/storage/pool";
import { WakeEventsPage } from "@/routes/storage/wake-events";
import { StorageSetupPage } from "@/routes/storage-setup";
import { BackupSettingsPage } from "@/routes/settings/backup";
import { GeneralSettingsPage } from "@/routes/settings/general";
import { NotificationsSettingsPage } from "@/routes/settings/notifications";
import { SchedulesSettingsPage } from "@/routes/settings/schedules";
import { UpdatesSettingsPage } from "@/routes/settings/updates";
import { NetworkSettingsPage } from "@/routes/settings/network";
import { UsersPage } from "@/routes/users";
import { WelcomePage } from "@/routes/welcome";

const STORAGE_NAV = (t: ReturnType<typeof useTranslation>["t"]) => [
  { to: "/storage", label: t("storageNav.pool") },
  { to: "/storage/disks", label: t("storageNav.disks") },
  { to: "/storage/disks/wake-events", label: t("storageNav.wakeEvents") },
  { to: "/storage/parity", label: t("storageNav.parity") },
  { to: "/storage/cache", label: t("storageNav.cache") },
  { to: "/storage/setup", label: t("storageNav.setup") },
];

const APPS_NAV = (t: ReturnType<typeof useTranslation>["t"]) => [
  { to: "/apps", label: t("appsNav.installed") },
  { to: "/apps/catalog", label: t("appsNav.catalog") },
];

const VMS_NAV = (t: ReturnType<typeof useTranslation>["t"]) => [
  { to: "/vms", label: t("vmsNav.list") },
  { to: "/vms/passthrough", label: t("vmsNav.passthrough") },
];

const SETTINGS_NAV = (t: ReturnType<typeof useTranslation>["t"]) => [
  { to: "/settings", label: t("settingsNav.general") },
  { to: "/settings/network", label: t("settingsNav.network") },
  { to: "/settings/notifications", label: t("settingsNav.notifications") },
  { to: "/settings/schedules", label: t("settingsNav.schedules") },
  { to: "/settings/backup", label: t("settingsNav.backup") },
  { to: "/settings/updates", label: t("settingsNav.updates") },
  { to: "/settings/advanced", label: t("settingsNav.advanced") },
];

const TOOLS_NAV = (t: ReturnType<typeof useTranslation>["t"]) => [
  { to: "/tools/logs", label: t("toolsNav.logs") },
  { to: "/tools/terminal", label: t("toolsNav.terminal") },
  { to: "/tools/migrate", label: t("toolsNav.migrate") },
  { to: "/tools/diagnostics", label: t("toolsNav.diagnostics") },
];

function AuthenticatedRoutes(): React.ReactElement {
  const { t } = useTranslation();

  return (
    <AppShell>
      <Routes>
        <Route index element={<DashboardPage />} />

        <Route element={<SectionLayout items={STORAGE_NAV(t)} />}>
          <Route path="storage" element={<PoolOverviewPage />} />
          <Route path="storage/disks" element={<DisksPage />} />
          <Route path="storage/disks/wake-events" element={<WakeEventsPage />} />
          <Route path="storage/parity" element={<ParityPage />} />
          <Route path="storage/cache" element={<CachePage />} />
          <Route path="storage/setup" element={<StorageSetupPage />} />
        </Route>
        <Route path="storage/disks/:diskId" element={<DiskDetailPage />} />

        <Route path="shares" element={<SharesPage />} />
        <Route path="shares/:name" element={<ShareDetailPage />} />

        <Route element={<SectionLayout items={APPS_NAV(t)} />}>
          <Route path="apps" element={<AppsPage />} />
          <Route path="apps/catalog" element={<CatalogPage />} />
        </Route>
        <Route path="apps/catalog/:appId" element={<CatalogDetailPage />} />
        <Route path="apps/install/:appId" element={<InstallPage />} />
        <Route path="apps/:name" element={<AppDetailPage />} />
        <Route path="apps/:name/compose" element={<ComposePage />} />

        <Route element={<SectionLayout items={VMS_NAV(t)} />}>
          <Route path="vms" element={<PlaceholderPage titleKey="vmsNav.list" />} />
          <Route path="vms/passthrough" element={<PlaceholderPage titleKey="vmsNav.passthrough" />} />
        </Route>
        <Route path="vms/create" element={<PlaceholderPage titleKey="vmsNav.create" />} />
        <Route path="vms/:name" element={<PlaceholderPage titleKey="vmsNav.list" />} />

        <Route path="jobs" element={<JobsPage />} />
        <Route path="jobs/:jobId" element={<JobDetailPage />} />

        <Route path="users" element={<UsersPage />} />

        <Route element={<SectionLayout items={SETTINGS_NAV(t)} />}>
          <Route path="settings" element={<GeneralSettingsPage />} />
          <Route path="settings/network" element={<NetworkSettingsPage />} />
          <Route path="settings/notifications" element={<NotificationsSettingsPage />} />
          <Route path="settings/schedules" element={<SchedulesSettingsPage />} />
          <Route path="settings/backup" element={<BackupSettingsPage />} />
          <Route path="settings/updates" element={<UpdatesSettingsPage />} />
          <Route path="settings/advanced" element={<PlaceholderPage titleKey="settingsNav.advanced" />} />
        </Route>

        <Route element={<SectionLayout items={TOOLS_NAV(t)} />}>
          <Route path="tools/logs" element={<PlaceholderPage titleKey="toolsNav.logs" />} />
          <Route path="tools/terminal" element={<PlaceholderPage titleKey="toolsNav.terminal" />} />
          <Route path="tools/migrate" element={<PlaceholderPage titleKey="toolsNav.migrate" />} />
          <Route path="tools/diagnostics" element={<PlaceholderPage titleKey="toolsNav.diagnostics" />} />
        </Route>
      </Routes>
    </AppShell>
  );
}

function AppRoutes(): React.ReactElement {
  return (
    <AuthProvider>
      <AuthGate>
        <Routes>
          <Route
            path="/welcome"
            element={
              <MinimalAuthLayout>
                <WelcomePage />
              </MinimalAuthLayout>
            }
          />
          <Route
            path="/login"
            element={
              <MinimalAuthLayout>
                <LoginPage />
              </MinimalAuthLayout>
            }
          />
          <Route path="/*" element={<AuthenticatedRoutes />} />
        </Routes>
      </AuthGate>
    </AuthProvider>
  );
}

// A data router, because useBlocker (the unsaved-changes guard on in-app
// navigation) needs one. The route tree stays in AppRoutes' own <Routes>.
export function App(): React.ReactElement {
  useEffect(() => watchSystemTheme(), []);
  const [router] = useState(() => createBrowserRouter([{ path: "*", element: <AppRoutes /> }]));

  return <RouterProvider router={router} />;
}
