import { useEffect } from "react";
import { useTranslation } from "react-i18next";
import { Route, BrowserRouter, Routes } from "react-router-dom";

import { AppShell } from "@/components/patterns/app-shell";
import { AuthGate, AuthProvider, MinimalAuthLayout } from "@/lib/api/auth-guard";
import { watchSystemTheme } from "@/lib/theme";
import { JobsPage } from "@/routes/jobs";
import { LoginPage } from "@/routes/login";
import { PlaceholderPage } from "@/routes/placeholder-page";
import { SectionLayout } from "@/routes/section-layout";
import { StorageSetupPage } from "@/routes/storage-setup";
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
        <Route index element={<PlaceholderPage titleKey="nav.dashboard" />} />

        <Route element={<SectionLayout items={STORAGE_NAV(t)} />}>
          <Route path="storage" element={<PlaceholderPage titleKey="storageNav.pool" />} />
          <Route path="storage/disks" element={<PlaceholderPage titleKey="storageNav.disks" />} />
          <Route
            path="storage/disks/wake-events"
            element={<PlaceholderPage titleKey="storageNav.wakeEvents" />}
          />
          <Route path="storage/parity" element={<PlaceholderPage titleKey="storageNav.parity" />} />
          <Route path="storage/cache" element={<PlaceholderPage titleKey="storageNav.cache" />} />
          <Route path="storage/setup" element={<StorageSetupPage />} />
        </Route>
        <Route path="storage/disks/:diskId" element={<PlaceholderPage titleKey="storageNav.disks" />} />

        <Route path="shares" element={<PlaceholderPage titleKey="nav.shares" />} />
        <Route path="shares/:name" element={<PlaceholderPage titleKey="nav.shares" />} />

        <Route element={<SectionLayout items={APPS_NAV(t)} />}>
          <Route path="apps" element={<PlaceholderPage titleKey="appsNav.installed" />} />
          <Route path="apps/catalog" element={<PlaceholderPage titleKey="appsNav.catalog" />} />
        </Route>
        <Route path="apps/catalog/:appId" element={<PlaceholderPage titleKey="appsNav.catalog" />} />
        <Route path="apps/install/:appId" element={<PlaceholderPage titleKey="appsNav.installed" />} />
        <Route path="apps/:name" element={<PlaceholderPage titleKey="appsNav.installed" />} />
        <Route path="apps/:name/compose" element={<PlaceholderPage titleKey="appsNav.installed" />} />

        <Route element={<SectionLayout items={VMS_NAV(t)} />}>
          <Route path="vms" element={<PlaceholderPage titleKey="vmsNav.list" />} />
          <Route path="vms/passthrough" element={<PlaceholderPage titleKey="vmsNav.passthrough" />} />
        </Route>
        <Route path="vms/create" element={<PlaceholderPage titleKey="vmsNav.create" />} />
        <Route path="vms/:name" element={<PlaceholderPage titleKey="vmsNav.list" />} />

        <Route path="jobs" element={<JobsPage />} />
        <Route path="jobs/:jobId" element={<JobsPage />} />

        <Route path="users" element={<PlaceholderPage titleKey="nav.users" />} />

        <Route element={<SectionLayout items={SETTINGS_NAV(t)} />}>
          <Route path="settings" element={<PlaceholderPage titleKey="settingsNav.general" />} />
          <Route path="settings/network" element={<PlaceholderPage titleKey="settingsNav.network" />} />
          <Route
            path="settings/notifications"
            element={<PlaceholderPage titleKey="settingsNav.notifications" />}
          />
          <Route path="settings/schedules" element={<PlaceholderPage titleKey="settingsNav.schedules" />} />
          <Route path="settings/backup" element={<PlaceholderPage titleKey="settingsNav.backup" />} />
          <Route path="settings/updates" element={<PlaceholderPage titleKey="settingsNav.updates" />} />
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

export function App(): React.ReactElement {
  useEffect(() => watchSystemTheme(), []);

  return (
    <BrowserRouter>
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
    </BrowserRouter>
  );
}
