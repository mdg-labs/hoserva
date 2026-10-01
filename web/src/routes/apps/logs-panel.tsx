import { RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { FormOverlay } from "@/components/patterns/form-overlay";
import { LogView } from "@/components/patterns/log-view";
import { Button } from "@/components/ui/button";
import { getAppLogs } from "@/lib/api/operations";
import { useApiQuery } from "@/lib/api/use-api-query";
import type { App } from "@/routes/apps/containers";

const LOG_TAIL_LINES = 200;

// A drawer on a phone and a dialog on a desktop (form-overlay), so a
// container's recent output is readable and refreshable at either width.
export function LogsPanel({
  app,
  onClose,
}: {
  app: App | null;
  onClose: () => void;
}): React.ReactElement {
  const { t } = useTranslation();
  const logsQuery = useApiQuery<string>({
    queryKey: ["app-logs", app?.id ?? null],
    queryFn: (signal) => getAppLogs(app?.id ?? "", LOG_TAIL_LINES, signal),
    enabled: app !== null,
    fallbackError: t("apps.logs.loadFailed"),
  });

  return (
    <FormOverlay
      open={app !== null}
      onOpenChange={(open) => {
        if (!open) {
          onClose();
        }
      }}
      title={t("apps.logs.title", { name: app?.name ?? "" })}
      description={t("apps.logs.description", { count: LOG_TAIL_LINES })}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="outline" loading={logsQuery.refreshing} onClick={() => void logsQuery.refresh()}>
            <RefreshCw aria-hidden="true" />
            {t("apps.logs.refresh")}
          </Button>
          <Button variant="outline" onClick={onClose}>
            {t("apps.logs.close")}
          </Button>
        </div>
      }
    >
      {logsQuery.error ? <Banner tone="error" title={logsQuery.error} /> : null}
      {logsQuery.error && logsQuery.data === null ? null : (
        <LogView
          content={logsQuery.data === "" ? t("apps.logs.empty") : logsQuery.data}
          loading={logsQuery.loading}
        />
      )}
    </FormOverlay>
  );
}
