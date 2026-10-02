import { PackageSearch, RefreshCw, Search } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { EmptyState } from "@/components/patterns/empty-state";
import { showFeedbackToast } from "@/components/patterns/feedback-toast";
import { LoadingBlock } from "@/components/patterns/loading";
import { MultiPick } from "@/components/patterns/multi-pick";
import { Pagination } from "@/components/patterns/pagination";
import { SourceBadge } from "@/components/patterns/source-badge";
import { StatusBadge } from "@/components/patterns/status-badge";
import { TemplateIcon } from "@/components/patterns/template-icon";
import { ToggleFilter } from "@/components/patterns/toggle-filter";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardPanel } from "@/components/ui/card";
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group";
import { subscribeToEvents } from "@/lib/api/events";
import type { components } from "@/lib/api/client";
import { getCatalog, postCatalogRefresh } from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import {
  catalogDetailPath,
  COMMUNITY,
  entryCategories,
  filterEntries,
  hasFilters,
  INSTALLED,
  NO_FILTERS,
  NOT_INSTALLED,
  VERIFIED,
  type CatalogEntry,
  type CatalogFilters,
} from "@/routes/apps/catalog-filter";

type CatalogList = components["schemas"]["CatalogList"];
type CatalogRefresh = components["schemas"]["CatalogRefresh"];

const PAGE_SIZES = [12, 24, 48];

function checkToast(
  result: CatalogRefresh,
  t: (key: string, options?: Record<string, unknown>) => string,
): { type: "success" | "error"; title: string; description?: string } {
  if (result.outcome === "updated") {
    return {
      type: "success",
      title: t("apps.catalog.check.updated", {
        new: result.newTemplates ?? 0,
        updated: result.updatedTemplates ?? 0,
      }),
    };
  }
  if (result.outcome === "unchanged") {
    return { type: "success", title: t("apps.catalog.check.unchanged") };
  }
  return {
    type: "error",
    title: t("apps.catalog.check.failed"),
    description:
      result.message ??
      (result.reason ? t(`apps.catalog.check.reasons.${result.reason}`) : t("apps.catalog.check.failedUnknown")),
  };
}

function CatalogCard({ entry }: { entry: CatalogEntry }): React.ReactElement {
  const { t } = useTranslation();
  return (
    <Card className="relative w-full transition-colors focus-within:ring-2 focus-within:ring-ring hover:bg-accent/30">
      <CardPanel className="flex h-full flex-col gap-3">
        <div className="flex items-start gap-3">
          <TemplateIcon id={entry.id} title={entry.title} className="shrink-0" />
          <div className="flex min-w-0 flex-col gap-1">
            <Link
              to={catalogDetailPath(entry.id)}
              className="truncate font-heading font-semibold outline-none after:absolute after:inset-0"
            >
              {entry.title}
            </Link>
            <div className="flex flex-wrap gap-1">
              <SourceBadge kind={entry.sourceKind} signed={entry.signed} />
              {entry.installed ? <StatusBadge tone="outline">{t("apps.catalog.installed")}</StatusBadge> : null}
            </div>
          </div>
        </div>
        <div className="flex flex-wrap gap-1">
          {entry.categories.map((category) => (
            <Badge key={category} variant="outline">
              {t(`apps.catalog.categories.${category}`, { defaultValue: category })}
            </Badge>
          ))}
        </div>
      </CardPanel>
    </Card>
  );
}

export function CatalogPage(): React.ReactElement {
  const { t } = useTranslation();
  const [filters, setFilters] = useState<CatalogFilters>(NO_FILTERS);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(PAGE_SIZES[0]);
  const checking = useRef(false);

  const catalogQuery = useApiQuery<CatalogList>({
    queryKey: "catalog",
    queryFn: (signal) => getCatalog(signal),
    fallbackError: t("apps.catalog.loadFailed"),
  });
  const refreshMutation = useApiMutation<void, CatalogRefresh>({
    mutationFn: () => postCatalogRefresh(),
    fallbackError: t("apps.catalog.check.failedUnknown"),
  });

  const refreshList = useRef(catalogQuery.refresh);
  useEffect(() => {
    refreshList.current = catalogQuery.refresh;
  }, [catalogQuery.refresh]);

  useEffect(() => {
    return subscribeToEvents((event) => {
      if (event.event === "catalog") {
        void refreshList.current();
      }
    });
  }, []);

  const catalog = catalogQuery.data;
  const entries = useMemo(() => catalog?.templates ?? [], [catalog]);
  const categories = useMemo(() => entryCategories(entries), [entries]);
  const matches = useMemo(() => filterEntries(entries, filters), [entries, filters]);

  const pageCount = Math.max(1, Math.ceil(matches.length / pageSize));
  const currentPage = Math.min(page, pageCount);
  const visible = matches.slice((currentPage - 1) * pageSize, currentPage * pageSize);

  function changeFilters(next: Partial<CatalogFilters>): void {
    setFilters((current) => ({ ...current, ...next }));
    setPage(1);
  }

  async function checkForUpdates(): Promise<void> {
    if (checking.current) {
      return;
    }
    checking.current = true;
    try {
      const result = await refreshMutation.mutate();
      if (!result.ok) {
        if (!result.aborted) {
          showFeedbackToast({
            type: "error",
            title: t("apps.catalog.check.failed"),
            description: result.error,
          });
        }
        return;
      }
      if (result.data === undefined) {
        showFeedbackToast({
          type: "error",
          title: t("apps.catalog.check.failed"),
          description: t("apps.catalog.check.failedUnknown"),
        });
        return;
      }
      showFeedbackToast(checkToast(result.data, t));
      await catalogQuery.refresh();
    } finally {
      checking.current = false;
    }
  }

  let body: React.ReactElement | null;
  if (catalog === null) {
    body = catalogQuery.error ? null : <LoadingBlock />;
  } else if (entries.length === 0) {
    body = (
      <EmptyState
        icon={PackageSearch}
        title={t("apps.catalog.empty.title")}
        description={t("apps.catalog.empty.description")}
      />
    );
  } else if (matches.length === 0) {
    body = (
      <EmptyState
        icon={PackageSearch}
        title={t("apps.catalog.noMatches.title")}
        description={t("apps.catalog.noMatches.description")}
        action={
          hasFilters(filters) ? (
            <Button
              variant="outline"
              onClick={() => {
                setFilters(NO_FILTERS);
                setPage(1);
              }}
            >
              {t("apps.catalog.noMatches.clear")}
            </Button>
          ) : undefined
        }
      />
    );
  } else {
    body = (
      <div className="flex flex-col gap-4">
        <ul className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
          {visible.map((entry) => (
            <li key={entry.id} className="flex">
              <CatalogCard entry={entry} />
            </li>
          ))}
        </ul>
        <Pagination
          page={currentPage}
          pageSize={pageSize}
          pageSizes={PAGE_SIZES}
          total={matches.length}
          onPageChange={setPage}
          onPageSizeChange={(size) => {
            setPageSize(size);
            setPage(1);
          }}
        />
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="font-heading text-2xl font-semibold">{t("apps.catalog.title")}</h1>
          <p className="text-muted-foreground">{t("apps.catalog.description")}</p>
        </div>
        <div className="flex flex-wrap items-center gap-3">
          {catalog ? (
            <div className="flex flex-wrap items-center gap-2 text-muted-foreground text-sm">
              <span>
                {catalog.lastCheckedAt
                  ? t("apps.catalog.lastChecked", { time: new Date(catalog.lastCheckedAt).toLocaleString() })
                  : t("apps.catalog.neverChecked")}
              </span>
              {catalog.lastOutcome === "failed" ? (
                <StatusBadge tone="warning">{t("apps.catalog.lastCheckFailed")}</StatusBadge>
              ) : null}
            </div>
          ) : null}
          <Button
            variant="outline"
            loading={refreshMutation.pending}
            onClick={() => void checkForUpdates()}
          >
            <RefreshCw aria-hidden="true" />
            {t("apps.catalog.check.action")}
          </Button>
        </div>
      </div>

      {catalogQuery.error ? (
        <Banner
          tone={catalog ? "warning" : "error"}
          title={catalogQuery.error}
          action={
            <Button size="xs" variant="outline" onClick={() => void catalogQuery.refresh()}>
              {t("apps.installed.retry")}
            </Button>
          }
        />
      ) : null}

      {catalog && entries.length > 0 ? (
        <div className="flex flex-col gap-3">
          <InputGroup>
            <InputGroupAddon>
              <Search aria-hidden="true" />
            </InputGroupAddon>
            <InputGroupInput
              aria-label={t("apps.catalog.search")}
              placeholder={t("apps.catalog.search")}
              value={filters.search}
              onChange={(event) => changeFilters({ search: event.target.value })}
            />
          </InputGroup>
          <div className="flex flex-col gap-3 md:flex-row md:items-start">
            <div className="min-w-0 flex-1">
              <MultiPick
                value={filters.categories}
                onChange={(next) => changeFilters({ categories: next })}
                options={categories.map((category) => ({
                  value: category,
                  label: t(`apps.catalog.categories.${category}`, { defaultValue: category }),
                }))}
                placeholder={t("apps.catalog.categoryFilter")}
                label={t("apps.catalog.categoryFilter")}
              />
            </div>
            <div className="flex flex-wrap gap-2">
              <ToggleFilter
                label={t("apps.catalog.installedFilter")}
                value={filters.installed}
                onChange={(installed) => changeFilters({ installed })}
                options={[
                  { value: INSTALLED, label: t("apps.catalog.installed") },
                  { value: NOT_INSTALLED, label: t("apps.catalog.notInstalled") },
                ]}
              />
              <ToggleFilter
                label={t("apps.catalog.verifiedFilter")}
                value={filters.verified}
                onChange={(verified) => changeFilters({ verified })}
                options={[
                  { value: VERIFIED, label: t("apps.catalog.verified") },
                  { value: COMMUNITY, label: t("apps.catalog.community") },
                ]}
              />
            </div>
          </div>
        </div>
      ) : null}

      {body}
    </div>
  );
}
