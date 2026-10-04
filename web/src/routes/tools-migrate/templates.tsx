import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { LoadingBlock } from "@/components/patterns/loading";
import { StatusBadge } from "@/components/patterns/status-badge";
import { SelectFilter, TableFilters } from "@/components/patterns/table-filters";
import { Button } from "@/components/ui/button";
import type { components } from "@/lib/api/client";
import type { UseApiQueryResult } from "@/lib/api/use-api-query";
import {
  CLASS_FILTER_ALL,
  CLASS_FILTER_DEFAULT,
  TEMPLATE_CLASSES,
  classFilterMatches,
  flaggedContainers,
  reportRowKey,
  templateClassTone,
  templateStatusTone,
  type MigrationTemplates,
  type ReportRow,
} from "@/routes/tools-migrate/report";
import { reportRowColumns } from "@/routes/tools-migrate/columns";

type TemplateSummary = components["schemas"]["MigrationTemplateSummary"];
type ComposeProjectSummary = components["schemas"]["MigrationComposeProjectSummary"];

function TemplateStatusBadge({ item }: { item: TemplateSummary | ComposeProjectSummary }): React.ReactElement {
  const { t } = useTranslation();
  const tone = templateStatusTone(item.status);
  if (item.status === "warnings" && "warningCount" in item) {
    return <StatusBadge tone={tone}>{t("toolsMigrate.templates.warningCount", { count: item.warningCount })}</StatusBadge>;
  }
  return <StatusBadge tone={tone}>{t(`toolsMigrate.templates.status.${item.status}`, { defaultValue: item.status })}</StatusBadge>;
}

function errorNote(error: string | undefined): React.ReactElement | null {
  return error ? <p className="mt-1 text-muted-foreground text-xs">{error}</p> : null;
}

export function TemplatesPreview({
  rows,
  query,
}: {
  rows: ReportRow[];
  query: UseApiQueryResult<MigrationTemplates>;
}): React.ReactElement {
  const { t } = useTranslation();
  const [classFilter, setClassFilter] = useState(CLASS_FILTER_DEFAULT);
  const data = query.data;

  const templateColumns = useMemo<DataTableColumn<TemplateSummary>[]>(
    () => [
      {
        id: "name",
        header: t("toolsMigrate.templates.columns.name"),
        cell: (template) => (
          <div>
            <span className="font-medium">{template.name}</span>
            <span className="block font-mono text-muted-foreground text-xs">{template.file}</span>
          </div>
        ),
      },
      {
        id: "class",
        header: t("toolsMigrate.templates.columns.class"),
        cell: (template) => (
          <StatusBadge tone={templateClassTone(template.class)}>
            {t(`toolsMigrate.templates.class.${template.class}`, { defaultValue: template.class })}
          </StatusBadge>
        ),
      },
      {
        id: "conversion",
        header: t("toolsMigrate.templates.columns.conversion"),
        cell: (template) => (
          <div>
            <TemplateStatusBadge item={template} />
            {errorNote(template.error)}
          </div>
        ),
        className: "whitespace-normal",
      },
    ],
    [t],
  );
  const projectColumns = useMemo<DataTableColumn<ComposeProjectSummary>[]>(
    () => [
      { id: "name", header: t("toolsMigrate.templates.columns.project"), cell: (project) => <span className="font-medium">{project.name}</span> },
      {
        id: "containers",
        header: t("toolsMigrate.templates.columns.containers"),
        cell: (project) => project.containers.join(", "),
        className: "whitespace-normal",
      },
      {
        id: "status",
        header: t("toolsMigrate.templates.columns.conversion"),
        cell: (project) => (
          <div>
            <TemplateStatusBadge item={project} />
            {errorNote(project.error)}
          </div>
        ),
        className: "whitespace-normal",
      },
    ],
    [t],
  );
  const flaggedColumns = useMemo(() => reportRowColumns(t), [t]);
  const flagged = useMemo(() => flaggedContainers(rows), [rows]);

  const classOptions = useMemo(
    () => [
      { value: CLASS_FILTER_DEFAULT, label: t("toolsMigrate.templates.filter.default") },
      { value: CLASS_FILTER_ALL, label: t("toolsMigrate.templates.filter.all") },
      ...TEMPLATE_CLASSES.map((value) => ({ value, label: t(`toolsMigrate.templates.class.${value}`) })),
    ],
    [t],
  );
  const visibleTemplates = useMemo(
    () => (data?.templates ?? []).filter((template) => classFilterMatches(classFilter, template.class)),
    [data, classFilter],
  );

  return (
    <section className="flex flex-col gap-4" aria-labelledby="migration-templates-title">
      <h3 id="migration-templates-title" className="font-medium">
        {t("toolsMigrate.templates.title")}
      </h3>
      {query.error ? (
        <Banner
          tone="error"
          title={query.error}
          action={
            <Button size="xs" variant="outline" onClick={() => void query.refresh()}>
              {t("toolsMigrate.retry")}
            </Button>
          }
        />
      ) : null}
      {query.loading ? <LoadingBlock rows={2} /> : null}
      {data ? (
        <>
          <p className="text-muted-foreground text-sm">
            {data.counts.allTemplates
              ? t("toolsMigrate.templates.countsAll", {
                  clean: data.counts.clean,
                  withWarnings: data.counts.withWarnings,
                  failed: data.counts.failed,
                })
              : t("toolsMigrate.templates.counts", {
                  clean: data.counts.clean,
                  withWarnings: data.counts.withWarnings,
                  failed: data.counts.failed,
                  templateOnly: data.counts.templateOnly,
                })}
          </p>
          <TableFilters
            filters={
              <SelectFilter
                value={classFilter}
                onChange={setClassFilter}
                placeholder={t("toolsMigrate.templates.filter.label")}
                options={classOptions}
              />
            }
          />
          <DataTable rows={visibleTemplates} getRowKey={(template) => template.file} columns={templateColumns} />
          {visibleTemplates.length === 0 ? (
            <p className="text-muted-foreground text-sm">
              {data.templates.length === 0
                ? t("toolsMigrate.templates.none")
                : t("toolsMigrate.templates.noneMatching")}
            </p>
          ) : null}
          {data.composeProjects.length > 0 ? (
            <div className="flex flex-col gap-2">
              <h4 className="font-medium text-sm">{t("toolsMigrate.templates.projectsTitle")}</h4>
              <p className="text-muted-foreground text-sm">{t("toolsMigrate.templates.projectsDescription")}</p>
              <DataTable rows={data.composeProjects} getRowKey={(project) => project.name} columns={projectColumns} />
            </div>
          ) : null}
        </>
      ) : null}
      {flagged.length > 0 ? (
        <div className="flex flex-col gap-2">
          <h4 className="font-medium text-sm">{t("toolsMigrate.templates.flaggedTitle")}</h4>
          <p className="text-muted-foreground text-sm">{t("toolsMigrate.templates.flaggedDescription")}</p>
          <DataTable rows={flagged} getRowKey={reportRowKey} columns={flaggedColumns} />
        </div>
      ) : null}
    </section>
  );
}
