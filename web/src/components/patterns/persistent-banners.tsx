import type React from "react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { ConfirmDialog } from "@/components/patterns/confirm";
import { Button } from "@/components/ui/button";
import { PATHS } from "@/hooks/paths";
import { useSystemData } from "@/hooks/use-system-status";
import { postAcknowledgeDegradedArray } from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";

export function PersistentBanners(): React.ReactElement {
  const { t } = useTranslation();
  const { status, pool, doctor, jobs, refresh } = useSystemData();
  const [acknowledgeOpen, setAcknowledgeOpen] = useState(false);
  const acknowledgeMutation = useApiMutation({ mutationFn: postAcknowledgeDegradedArray });

  const missingDisks = (pool?.disks ?? []).filter((disk) => disk.state === "missing");

  async function handleAcknowledge(): Promise<void> {
    const result = await acknowledgeMutation.mutate(undefined);
    if (!result.ok) {
      return;
    }
    setAcknowledgeOpen(false);
    await refresh();
  }

  const banners: React.ReactElement[] = [];

  if (status?.arrayDegraded) {
    // arrayDegraded stays true for as long as a disk is actually missing,
    // even once the user has acknowledged it (the API never reports a
    // degraded array as healthy) — arrayDegradedAcknowledged is what
    // distinguishes "acknowledged, running degraded" from "not yet
    // acknowledged", so the banner swaps its action instead of
    // disappearing outright. arrayDegradedAcknowledged alone is not
    // enough, though: it can be true while the transition it triggered
    // did not actually start anything (maintenance mode, or a mount
    // failure — array_services_not_started) — a later poll then reports
    // the acknowledgement without the services actually running (#385
    // finding 2). storageServicesReleased is what the banner also
    // requires before it ever says services are running; until then it
    // keeps the acknowledge/retry action, since that is what a refused
    // acknowledge needs to recover from.
    const acknowledged =
      Boolean(status.arrayDegradedAcknowledged) && Boolean(status.storageServicesReleased);
    banners.push(
      <Banner
        key="degraded"
        tone={acknowledged ? "warning" : "error"}
        title={t(acknowledged ? "banners.degraded.acknowledgedTitle" : "banners.degraded.title")}
        description={t(
          acknowledged ? "banners.degraded.acknowledgedDescription" : "banners.degraded.description",
        )}
        action={
          <>
            <Button size="sm" variant="outline" render={<Link to={PATHS.storageDisks} />}>
              {t("banners.degraded.action")}
            </Button>
            {acknowledged ? null : (
              <Button
                size="sm"
                onClick={() => {
                  acknowledgeMutation.reset();
                  setAcknowledgeOpen(true);
                }}
              >
                {t("banners.degraded.acknowledgeAction")}
              </Button>
            )}
          </>
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
  if (driftCheck?.status === "warn" || driftCheck?.status === "fail") {
    banners.push(
      <Banner
        key="drift"
        tone="warning"
        title={t("banners.drift.title")}
        description={driftCheck.message}
      />,
    );
  }

  return (
    <>
      {banners.length > 0 ? <div className="flex flex-col gap-3">{banners}</div> : null}
      <ConfirmDialog
        open={acknowledgeOpen}
        onOpenChange={(open) => {
          // Escape and a backdrop click both come through here as
          // onOpenChange(false), so a busy handler must ignore them too
          // (#375, #376, #385 finding 6) — otherwise a later refusal or
          // failure lands on a dialog the user already closed.
          if (!open && acknowledgeMutation.pending) {
            return;
          }
          setAcknowledgeOpen(open);
        }}
        title={t("banners.degraded.confirmTitle")}
        description={t("banners.degraded.confirmDescription", {
          count: missingDisks.length || 1,
        })}
        items={missingDisks.map((disk) =>
          t("banners.degraded.missingDiskItem", {
            mountPoint: disk.mountPoint,
            device: disk.device,
          }),
        )}
        error={acknowledgeMutation.error}
        loading={acknowledgeMutation.pending}
        confirmLabel={t("banners.degraded.confirmAction")}
        onConfirm={() => void handleAcknowledge()}
      />
    </>
  );
}
