import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { InlineNote } from "@/components/patterns/inline-note";
import { LoadingBlock } from "@/components/patterns/loading";
import { StatusBadge } from "@/components/patterns/status-badge";
import { buttonVariants } from "@/components/ui/button";
import type { components } from "@/lib/api/client";
import { getShares, getUsers } from "@/lib/api/operations";
import { useApiQuery } from "@/lib/api/use-api-query";
import { FailedJobBanner, MigrationJob } from "@/routes/tools-migrate/job-view";
import { jobActive, type Job } from "@/routes/tools-migrate/report";

type Share = components["schemas"]["Share"];
type User = components["schemas"]["UserSummary"];

const USERS_ROUTE = "/users";

function SeededShares(): React.ReactElement {
  const { t } = useTranslation();
  const shares = useApiQuery({
    queryKey: "migration-seeded-shares",
    queryFn: (signal) => getShares(signal),
    fallbackError: t("toolsMigrate.import.shares.loadFailed"),
  });
  const seeded = (shares.data?.shares ?? []).filter((share) => share.migration !== undefined);
  const columns: DataTableColumn<Share>[] = [
    { id: "name", header: t("toolsMigrate.import.shares.columns.name"), cell: (share) => <span className="font-medium">{share.name}</span> },
    {
      id: "cache",
      header: t("toolsMigrate.import.shares.columns.cache"),
      cell: (share) => {
        const target = share.migration?.targetCacheMode;
        return target
          ? t("toolsMigrate.import.shares.cacheLater", { mode: t(`shares.cacheModes.${target}.label`, { defaultValue: target }) })
          : t("toolsMigrate.import.shares.cacheNow", { mode: t(`shares.cacheModes.${share.cacheMode}.label`, { defaultValue: share.cacheMode }) });
      },
      className: "whitespace-normal",
    },
    {
      id: "notes",
      header: t("toolsMigrate.import.shares.columns.notes"),
      cell: (share) =>
        share.migration && share.migration.notes.length > 0 ? (
          <ul className="list-disc ps-4">
            {share.migration.notes.map((note) => (
              <li key={note}>{note}</li>
            ))}
          </ul>
        ) : (
          <StatusBadge tone="success">{t("toolsMigrate.import.shares.noNotes")}</StatusBadge>
        ),
      className: "whitespace-normal",
    },
  ];

  return (
    <section className="flex flex-col gap-2" aria-label={t("toolsMigrate.import.shares.title")}>
      <h3 className="font-medium">{t("toolsMigrate.import.shares.title")}</h3>
      <p className="text-muted-foreground text-sm">{t("toolsMigrate.import.shares.description")}</p>
      {shares.error ? <Banner tone="error" title={shares.error} /> : null}
      {shares.loading ? <LoadingBlock rows={2} /> : null}
      {shares.data && seeded.length > 0 ? <DataTable rows={seeded} getRowKey={(share) => share.name} columns={columns} /> : null}
      {shares.data && seeded.length === 0 ? (
        <p className="text-muted-foreground text-sm">{t("toolsMigrate.import.shares.none")}</p>
      ) : null}
    </section>
  );
}

function SeededUsers(): React.ReactElement {
  const { t } = useTranslation();
  const users = useApiQuery({
    queryKey: "migration-seeded-users",
    queryFn: (signal) => getUsers(signal),
    fallbackError: t("toolsMigrate.import.users.loadFailed"),
  });
  const pending = (users.data?.users ?? []).filter((user: User) => user.role === "share-only" && !user.hasCredential);

  return (
    <section className="flex flex-col gap-2" aria-label={t("toolsMigrate.import.users.title")}>
      <h3 className="font-medium">{t("toolsMigrate.import.users.title")}</h3>
      <p className="text-muted-foreground text-sm">{t("toolsMigrate.import.users.description")}</p>
      {users.error ? <Banner tone="error" title={users.error} /> : null}
      {users.loading ? <LoadingBlock rows={1} /> : null}
      {users.data && pending.length > 0 ? (
        <>
          <ul className="list-disc ps-5 text-sm">
            {pending.map((user) => (
              <li key={user.id}>{t("toolsMigrate.import.users.item", { name: user.username })}</li>
            ))}
          </ul>
          <div>
            <Link to={USERS_ROUTE} className={buttonVariants({ size: "sm", variant: "outline" })}>
              {t("toolsMigrate.import.users.action")}
            </Link>
          </div>
        </>
      ) : null}
      {users.data && pending.length === 0 ? (
        <p className="text-muted-foreground text-sm">{t("toolsMigrate.import.users.none")}</p>
      ) : null}
    </section>
  );
}

export function ImportStep({
  adopted,
  job,
  onChanged,
}: {
  adopted: boolean;
  job: Job | undefined;
  onChanged: () => void;
}): React.ReactElement {
  const { t } = useTranslation();

  if (jobActive(job) && job) {
    return (
      <div className="flex flex-col gap-4">
        <InlineNote description={t("toolsMigrate.import.running")} />
        <MigrationJob job={job} onChanged={onChanged} />
      </div>
    );
  }
  if (adopted) {
    return (
      <div className="flex flex-col gap-6">
        <InlineNote
          title={t("toolsMigrate.import.doneTitle")}
          description={t("toolsMigrate.import.doneDescription")}
        />
        <SeededShares />
        <SeededUsers />
      </div>
    );
  }
  if (job && job.status !== "succeeded") {
    return <FailedJobBanner job={job} title={t("toolsMigrate.import.failedTitle")} />;
  }
  return (
    <div className="flex flex-col gap-4">
      <InlineNote description={t("toolsMigrate.import.waiting")} />
      <LoadingBlock rows={1} />
    </div>
  );
}
