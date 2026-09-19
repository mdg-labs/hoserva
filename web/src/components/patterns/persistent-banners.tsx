import type React from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { Button } from "@/components/ui/button";
import { PATHS } from "@/hooks/paths";
import { useSystemData } from "@/hooks/use-system-status";

export function PersistentBanners(): React.ReactElement | null {
  const { t } = useTranslation();
  const { status, doctor, jobs } = useSystemData();

  const banners: React.ReactElement[] = [];

  if (status?.arrayDegraded) {
    banners.push(
      <Banner
        key="degraded"
        tone="error"
        title={t("banners.degraded.title")}
        description={t("banners.degraded.description")}
        action={
          <Button size="sm" variant="outline" render={<Link to={PATHS.storageDisks} />}>
            {t("banners.degraded.action")}
          </Button>
        }
      />,
    );
  }

  if (status?.parityBlocked) {
    const failedSync = jobs.find(
      (job) => job.type === "sync" && job.status === "failed" && job.error?.message,
    );
    banners.push(
      <Banner
        key="sync-blocked"
        tone="warning"
        title={t("banners.syncBlocked.title")}
        description={failedSync?.error?.message ?? t("banners.syncBlocked.description")}
        action={
          <Button size="sm" variant="outline" render={<Link to={PATHS.storageParity} />}>
            {t("banners.syncBlocked.action")}
          </Button>
        }
      />,
    );
  }

  const dockerCheck = doctor?.checks.find((check) => check.id === "docker");
  if (dockerCheck?.status === "fail") {
    banners.push(
      <Banner
        key="docker"
        tone="warning"
        title={t("banners.docker.title")}
        description={dockerCheck.message}
      />,
    );
  }

  const driftCheck = doctor?.checks.find((check) => check.id.includes("drift"));
  if (driftCheck) {
    banners.push(
      <Banner
        key="drift"
        tone="warning"
        title={t("banners.drift.title")}
        description={driftCheck.message}
      />,
    );
  }

  if (banners.length === 0) {
    return null;
  }

  return <div className="flex flex-col gap-3">{banners}</div>;
}
