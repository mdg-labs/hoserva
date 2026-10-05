import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { InlineNote } from "@/components/patterns/inline-note";
import { StatusBadge } from "@/components/patterns/status-badge";
import { FailedJobBanner, MigrationJob } from "@/routes/tools-migrate/job-view";
import {
  VERIFY_LIST_KEYS,
  jobActive,
  scopeMatches,
  verifyGreen,
  type Job,
  type MigrationVerify,
  type VerifyScope,
} from "@/routes/tools-migrate/report";
import { formatBytes } from "@/routes/storage-setup/config-preview";

type ScopeKind = "disk" | "share";

interface ScopeRow {
  kind: ScopeKind;
  scope: VerifyScope;
}

function scopeLabel(t: TFunction, row: ScopeRow): string {
  if (row.kind === "share" && row.scope.name === "") {
    return t("toolsMigrate.verify.poolRoot");
  }
  return t(`toolsMigrate.verify.kinds.${row.kind}`, { name: row.scope.name });
}

function numbers(value: number, language: string): string {
  return value.toLocaleString(language);
}

function ScopeResult({ scope }: { scope: VerifyScope }): React.ReactElement {
  const { t } = useTranslation();

  if (scope.problem !== undefined) {
    return <StatusBadge tone="error">{t("toolsMigrate.verify.result.problem")}</StatusBadge>;
  }
  return scopeMatches(scope) ? (
    <StatusBadge tone="success">{t("toolsMigrate.verify.result.match")}</StatusBadge>
  ) : (
    <StatusBadge tone="error">{t("toolsMigrate.verify.result.mismatch")}</StatusBadge>
  );
}

function ComparisonTable({ verify }: { verify: MigrationVerify }): React.ReactElement {
  const { t, i18n } = useTranslation();
  const rows: ScopeRow[] = [
    ...(verify.disks ?? []).map((scope): ScopeRow => ({ kind: "disk", scope })),
    ...(verify.shares ?? []).map((scope): ScopeRow => ({ kind: "share", scope })),
  ];
  const language = i18n.language;
  if (rows.length === 0) {
    return <></>;
  }
  const columns: DataTableColumn<ScopeRow>[] = [
    { id: "name", header: t("toolsMigrate.verify.columns.name"), cell: (row) => <span className="font-medium">{scopeLabel(t, row)}</span> },
    {
      id: "files",
      header: t("toolsMigrate.verify.columns.files"),
      cell: ({ scope }) => (
        <div className="flex flex-col">
          <span>
            {t("toolsMigrate.verify.expectedFound", {
              expected: numbers(scope.expected.files, language),
              found: numbers(scope.found.files, language),
            })}
          </span>
          <span className="text-muted-foreground text-xs">
            {t("toolsMigrate.verify.linksAndSpecial", {
              links: t("toolsMigrate.verify.links", { count: scope.found.symlinks }),
              special: t("toolsMigrate.verify.special", { count: scope.found.special }),
            })}
          </span>
        </div>
      ),
      className: "whitespace-normal",
    },
    {
      id: "bytes",
      header: t("toolsMigrate.verify.columns.bytes"),
      cell: ({ scope }) =>
        t("toolsMigrate.verify.expectedFound", {
          expected: formatBytes(scope.expected.bytes),
          found: formatBytes(scope.found.bytes),
        }),
      className: "whitespace-normal",
    },
    {
      id: "checksums",
      header: t("toolsMigrate.verify.columns.checksums"),
      cell: ({ kind, scope }) =>
        kind === "disk"
          ? t("toolsMigrate.verify.checksumsDisk", {
              checked: numbers(scope.hashed, language),
              mismatches: t("toolsMigrate.verify.mismatches", { count: scope.checksumChanged.total }),
            })
          : t("toolsMigrate.verify.mismatches", { count: scope.checksumChanged.total }),
      className: "whitespace-normal",
    },
    { id: "result", header: t("toolsMigrate.verify.columns.result"), cell: ({ scope }) => <ScopeResult scope={scope} /> },
  ];

  return <DataTable rows={rows} getRowKey={(row) => `${row.kind}|${row.scope.name}`} columns={columns} />;
}

function Mismatches({ verify }: { verify: MigrationVerify }): React.ReactElement | null {
  const { t } = useTranslation();
  const failed: ScopeRow[] = [
    ...(verify.disks ?? []).map((scope): ScopeRow => ({ kind: "disk", scope })),
    ...(verify.shares ?? []).map((scope): ScopeRow => ({ kind: "share", scope })),
  ].filter((row) => !scopeMatches(row.scope));

  if (failed.length === 0) {
    return null;
  }
  return (
    <section className="flex flex-col gap-3" aria-label={t("toolsMigrate.verify.mismatchedFiles")}>
      <h3 className="font-medium">{t("toolsMigrate.verify.mismatchedFiles")}</h3>
      {failed.map((row) => (
        <div key={`${row.kind}|${row.scope.name}`} className="flex flex-col gap-2 text-sm">
          <h4 className="font-medium">{scopeLabel(t, row)}</h4>
          {row.scope.problem ? <p>{row.scope.problem}</p> : null}
          {VERIFY_LIST_KEYS.filter((key) => row.scope[key].total > 0).map((key) => {
            const list = row.scope[key];
            return (
              <div key={key}>
                <p className="text-muted-foreground">{t(`toolsMigrate.verify.lists.${key}`, { count: list.total })}</p>
                <ul className="list-disc ps-5 font-mono text-xs">
                  {list.paths.map((path) => (
                    <li key={path}>{path}</li>
                  ))}
                </ul>
                {list.total > list.paths.length ? (
                  <p className="text-muted-foreground text-xs">
                    {t("toolsMigrate.verify.notShown", { count: list.total - list.paths.length })}
                  </p>
                ) : null}
              </div>
            );
          })}
        </div>
      ))}
    </section>
  );
}

function Duplicates({ verify }: { verify: MigrationVerify }): React.ReactElement | null {
  const { t } = useTranslation();

  if (verify.duplicates === 0) {
    return null;
  }
  return (
    <InlineNote
      description={
        <div className="flex flex-col gap-1">
          <p>{t("toolsMigrate.verify.duplicates", { count: verify.duplicates })}</p>
          <ul className="list-disc ps-5 font-mono text-xs">
            {(verify.duplicateSample ?? []).map((duplicate) => (
              <li key={duplicate.path}>{t("toolsMigrate.verify.duplicateOn", { path: duplicate.path, disks: duplicate.disks.join(", ") })}</li>
            ))}
          </ul>
        </div>
      }
    />
  );
}

export function VerifyStep({
  verify,
  job,
  running,
  onChanged,
  children,
}: {
  verify: MigrationVerify | undefined;
  job: Job | undefined;
  running: boolean;
  onChanged: () => void;
  children?: React.ReactNode;
}): React.ReactElement {
  const { t } = useTranslation();

  if (running) {
    return (
      <div className="flex flex-col gap-4">
        <InlineNote description={t("toolsMigrate.verify.running")} />
        {job && jobActive(job) ? <MigrationJob job={job} onChanged={onChanged} /> : null}
      </div>
    );
  }
  if (verify === undefined) {
    return (
      <div className="flex flex-col gap-4">
        <InlineNote description={t("toolsMigrate.verify.intro")} />
        {job && job.status !== "succeeded" ? <FailedJobBanner job={job} title={t("toolsMigrate.verify.jobFailed")} /> : null}
      </div>
    );
  }

  const green = verifyGreen(verify);
  return (
    <div className="flex flex-col gap-4">
      {green ? (
        <div className="flex items-center gap-2">
          <StatusBadge tone="success">{t("toolsMigrate.verify.result.match")}</StatusBadge>
          <span className="text-sm">{t("toolsMigrate.verify.passed")}</span>
        </div>
      ) : (
        <Banner
          tone="error"
          title={t("toolsMigrate.verify.failed.title")}
          description={
            <div className="flex flex-col gap-2">
              <p>{verify.error ? t("toolsMigrate.verify.failed.notFinished", { error: verify.error }) : t("toolsMigrate.verify.failed.mismatch")}</p>
              <p>{t("toolsMigrate.verify.failed.stop")}</p>
            </div>
          }
        />
      )}
      <ComparisonTable verify={verify} />
      <Mismatches verify={verify} />
      <Duplicates verify={verify} />
      {green ? children : null}
    </div>
  );
}
