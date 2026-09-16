// `loading` (doc 03 "Shared patterns", `p-skeleton-1`, `p-button-41`): page
// loads use skeletons shaped like the content they replace; a wait scoped
// to one button uses its own `loading` prop instead of a page-wide spinner.
import { useTranslation } from "react-i18next";

import { Skeleton } from "@/components/ui/skeleton";

export function LoadingBlock({ rows = 3 }: { rows?: number }): React.ReactElement {
  const { t } = useTranslation();

  return (
    <div className="flex flex-col gap-3" role="status" aria-label={t("loading.label")}>
      {Array.from({ length: rows }, (_, index) => (
        <Skeleton key={index} className="h-10 w-full" />
      ))}
    </div>
  );
}
