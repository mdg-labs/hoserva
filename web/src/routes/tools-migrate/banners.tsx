import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { buttonVariants } from "@/components/ui/button";
import { DOCS_CAPTURE_URL, DOCS_UNPROTECTED_WINDOW_URL, type CaptureNotice } from "@/routes/tools-migrate/report";

function DocsLink({ href, label }: { href: string; label: string }): React.ReactElement {
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
      className={buttonVariants({ size: "xs", variant: "outline" })}
    >
      {label}
    </a>
  );
}

// No `onDismiss`: the window is real until the first parity sync finishes
// (doc 05 §5), so nothing the user does on this page clears the warning.
export function UnprotectedWindowBanner(): React.ReactElement {
  const { t } = useTranslation();

  return (
    <Banner
      tone="error"
      title={t("toolsMigrate.unprotectedWindow.title")}
      description={
        <div className="flex flex-col gap-2">
          <p>{t("toolsMigrate.unprotectedWindow.window")}</p>
          <p>{t("toolsMigrate.unprotectedWindow.backup")}</p>
          <p>{t("toolsMigrate.unprotectedWindow.rollback")}</p>
        </div>
      }
      action={<DocsLink href={DOCS_UNPROTECTED_WINDOW_URL} label={t("toolsMigrate.unprotectedWindow.docs")} />}
    />
  );
}

export function UnverifiedLayoutBanner(): React.ReactElement {
  const { t } = useTranslation();

  return (
    <Banner
      tone="warning"
      title={t("toolsMigrate.unverifiedLayout.title")}
      description={t("toolsMigrate.unverifiedLayout.description")}
    />
  );
}

export function CaptureWarningBanner({
  notice,
  allUnknown,
}: {
  notice: CaptureNotice;
  allUnknown: boolean;
}): React.ReactElement {
  const { t, i18n } = useTranslation();
  const stale = notice.state === "stale";
  const capturedAt =
    notice.state !== "row" && notice.capturedAt
      ? new Intl.DateTimeFormat(i18n.language, { dateStyle: "medium", timeStyle: "short" }).format(new Date(notice.capturedAt))
      : null;
  const detail =
    notice.state === "row"
      ? notice.detail
      : t(`toolsMigrate.captureWarning.state.${stale && capturedAt ? "staleDated" : notice.state}`, { date: capturedAt });

  return (
    <Banner
      tone="warning"
      title={stale ? t("toolsMigrate.captureWarning.staleTitle") : t("toolsMigrate.captureWarning.title")}
      description={
        <div className="flex flex-col gap-2">
          <p>{detail}</p>
          {allUnknown ? <p>{t("toolsMigrate.captureWarning.allUnknown")}</p> : null}
          <p>{t("toolsMigrate.captureWarning.description")}</p>
        </div>
      }
      action={<DocsLink href={DOCS_CAPTURE_URL} label={t("toolsMigrate.captureWarning.docs")} />}
    />
  );
}
