import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { InlineNote } from "@/components/patterns/inline-note";
import { LoadingBlock } from "@/components/patterns/loading";
import { TypedConfirm } from "@/components/patterns/typed-confirm";
import { buttonVariants } from "@/components/ui/button";
import { FailedJobBanner, MigrationJob } from "@/routes/tools-migrate/job-view";
import { jobActive, type Job, type ParityErase, type ParityInit } from "@/routes/tools-migrate/report";
import { formatBytes } from "@/routes/storage-setup/config-preview";

const PARITY_ROUTE = "/storage/parity";

function EraseItem({ erase }: { erase: ParityErase }): React.ReactElement {
  const { t } = useTranslation();
  const details = [erase.serial, erase.size === undefined ? undefined : formatBytes(erase.size)].filter(Boolean).join(", ");

  return (
    <>
      {t(erase.partition ? "toolsMigrate.parity.erase.partition" : "toolsMigrate.parity.erase.whole", {
        device: erase.device,
        role: t(`toolsMigrate.parity.erase.roles.${erase.role}`),
        details: details ? t("toolsMigrate.parity.erase.details", { details }) : "",
      })}
    </>
  );
}

// The confirmation string, the devices, the window and the rollback text are
// the server's, shown and submitted as given.
export function ParityConfirm({
  init,
  value,
  onChange,
  refusal,
  failedJob,
}: {
  init: ParityInit | undefined;
  value: string;
  onChange: (value: string) => void;
  refusal: string | null;
  failedJob: Job | undefined;
}): React.ReactElement {
  const { t } = useTranslation();

  if (init === undefined) {
    return <Banner tone="error" title={t("toolsMigrate.parity.unavailable")} />;
  }
  const items: React.ReactNode[] = [
    ...init.erases.map((erase) => <EraseItem key={`${erase.role}|${erase.device}`} erase={erase} />),
    init.unprotectedWindow,
    ...init.rollback,
  ];

  return (
    <div className="flex flex-col gap-4">
      {failedJob ? <FailedJobBanner job={failedJob} title={t("toolsMigrate.parity.jobFailed")} /> : null}
      {refusal ? <Banner tone="error" title={t("toolsMigrate.parity.refused")} description={refusal} /> : null}
      {init.problem !== undefined || init.confirmation === undefined ? (
        <Banner
          tone="error"
          title={t("toolsMigrate.parity.problem")}
          description={init.problem ?? t("toolsMigrate.parity.unavailable")}
        />
      ) : (
        <TypedConfirm
          phrase={init.confirmation}
          value={value}
          onChange={onChange}
          title={t(init.finishing ? "toolsMigrate.parity.finishTitle" : "toolsMigrate.parity.title")}
          description={t(init.finishing ? "toolsMigrate.parity.finishDescription" : "toolsMigrate.parity.description")}
          items={items}
        />
      )}
    </div>
  );
}

export function ParityProgress({
  parityJob,
  syncJob,
  syncError,
  syncLoading,
  onChanged,
}: {
  parityJob: Job;
  syncJob: Job | undefined;
  syncError: string | null;
  syncLoading: boolean;
  onChanged: () => void;
}): React.ReactElement {
  const { t } = useTranslation();

  if (jobActive(parityJob)) {
    return (
      <div className="flex flex-col gap-4">
        <InlineNote description={t("toolsMigrate.parity.running")} />
        <MigrationJob job={parityJob} onChanged={onChanged} />
      </div>
    );
  }
  return (
    <div className="flex flex-col gap-4">
      <InlineNote title={t("toolsMigrate.parity.formattedTitle")} description={t("toolsMigrate.parity.formatted")} />
      {syncError ? <Banner tone="error" title={syncError} /> : null}
      {syncJob === undefined ? (
        syncLoading ? (
          <LoadingBlock rows={1} />
        ) : (
          <InlineNote description={t("toolsMigrate.parity.syncWaiting")} />
        )
      ) : jobActive(syncJob) ? (
        <MigrationJob job={syncJob} onChanged={onChanged} />
      ) : syncJob.status === "succeeded" ? (
        <InlineNote title={t("toolsMigrate.parity.syncDoneTitle")} description={t("toolsMigrate.parity.syncDone")} />
      ) : (
        <Banner
          tone="error"
          title={t("toolsMigrate.parity.syncFailedTitle")}
          description={
            <div className="flex flex-col gap-2">
              <p>{syncJob.error?.message ?? t(`toolsMigrate.jobEnded.${syncJob.status}`, { defaultValue: syncJob.status })}</p>
              <p>{t("toolsMigrate.parity.syncFailed")}</p>
            </div>
          }
          action={
            <Link to={PARITY_ROUTE} className={buttonVariants({ size: "xs", variant: "outline" })}>
              {t("toolsMigrate.parity.openParity")}
            </Link>
          }
        />
      )}
    </div>
  );
}
