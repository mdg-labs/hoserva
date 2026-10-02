// `Pagination` with page size (doc 03 §5.2, `p-pagination-3`): previous and
// next with the range on show, and a page-size Select. The caller owns the
// page; this only reports what the reader asked for.
import { ChevronLeft, ChevronRight } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Select, SelectItem, SelectPopup, SelectTrigger, SelectValue } from "@/components/ui/select";

export function Pagination({
  page,
  pageSize,
  pageSizes,
  total,
  onPageChange,
  onPageSizeChange,
}: {
  page: number;
  pageSize: number;
  pageSizes: number[];
  total: number;
  onPageChange: (page: number) => void;
  onPageSizeChange: (pageSize: number) => void;
}): React.ReactElement {
  const { t } = useTranslation();
  const pageCount = Math.max(1, Math.ceil(total / pageSize));
  const first = total === 0 ? 0 : (page - 1) * pageSize + 1;
  const last = Math.min(total, page * pageSize);

  return (
    <nav
      aria-label={t("pagination.label")}
      className="flex flex-wrap items-center justify-between gap-3"
    >
      <p className="text-muted-foreground text-sm">{t("pagination.range", { first, last, total })}</p>
      <div className="flex flex-wrap items-center gap-2">
        <Select
          value={String(pageSize)}
          items={pageSizes.map((size) => ({ value: String(size), label: t("pagination.perPage", { count: size }) }))}
          onValueChange={(value) => value && onPageSizeChange(Number(value))}
        >
          <SelectTrigger aria-label={t("pagination.pageSize")} className="w-36">
            <SelectValue />
          </SelectTrigger>
          <SelectPopup>
            {pageSizes.map((size) => (
              <SelectItem key={size} value={String(size)}>
                {t("pagination.perPage", { count: size })}
              </SelectItem>
            ))}
          </SelectPopup>
        </Select>
        <Button
          variant="outline"
          size="sm"
          disabled={page <= 1}
          onClick={() => onPageChange(page - 1)}
        >
          <ChevronLeft aria-hidden="true" />
          {t("pagination.previous")}
        </Button>
        <span className="text-sm" aria-live="polite">
          {t("pagination.page", { page, pageCount })}
        </span>
        <Button
          variant="outline"
          size="sm"
          disabled={page >= pageCount}
          onClick={() => onPageChange(page + 1)}
        >
          {t("pagination.next")}
          <ChevronRight aria-hidden="true" />
        </Button>
      </div>
    </nav>
  );
}
