import { useTranslation } from "react-i18next";

import { ScrollArea } from "@/components/ui/scroll-area";

export function LogView({
  content,
  loading = false,
}: {
  content: string | null;
  loading?: boolean;
}): React.ReactElement {
  const { t } = useTranslation();

  if (loading) {
    return (
      <div className="rounded-lg border bg-muted/30 p-4 text-sm text-muted-foreground" role="status">
        {t("loading.label")}
      </div>
    );
  }

  return (
    <ScrollArea className="h-96 rounded-lg border bg-muted/20">
      <pre className="p-4 font-mono text-xs whitespace-pre-wrap break-all">
        {content ?? t("logView.empty")}
      </pre>
    </ScrollArea>
  );
}
