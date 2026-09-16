// `banner` (doc 03 "Shared patterns", `p-alert-3`, `p-alert-6`, `p-alert-7`):
// persistent banners (array degraded, sync blocked, config drift,
// prerequisite missing, update available) and inline warning blocks. A
// banner is dismissible only when the underlying condition clears — that
// decision belongs to the caller, which only renders this while the
// condition holds.
import { AlertTriangle, Info, OctagonAlert } from "lucide-react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { Alert, AlertAction, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";

export type BannerTone = "info" | "warning" | "error";

const TONE_ICON: Record<BannerTone, typeof Info> = {
  error: OctagonAlert,
  info: Info,
  warning: AlertTriangle,
};

export function Banner({
  tone,
  title,
  description,
  action,
  onDismiss,
}: {
  tone: BannerTone;
  title: ReactNode;
  description?: ReactNode;
  action?: ReactNode;
  onDismiss?: () => void;
}): React.ReactElement {
  const { t } = useTranslation();
  const Icon = TONE_ICON[tone];

  return (
    <Alert variant={tone}>
      <Icon aria-hidden="true" />
      <AlertTitle>{title}</AlertTitle>
      {description ? <AlertDescription>{description}</AlertDescription> : null}
      {action || onDismiss ? (
        <AlertAction>
          {action}
          {onDismiss ? (
            <Button size="xs" variant="ghost" onClick={onDismiss}>
              {t("banner.dismiss")}
            </Button>
          ) : null}
        </AlertAction>
      ) : null}
    </Alert>
  );
}
