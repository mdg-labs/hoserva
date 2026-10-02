// What the install plan could not carry out or needs a decision on, listed in
// the review before anything runs (doc 04 §5): untranslated flags, host paths
// outside the pool, clashes with the template's own entries, a network that
// does not exist and notes.
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { CopyValue } from "@/components/patterns/copy-value";
import type { ConversionWarning } from "@/routes/apps/install-form";

export function PlanWarnings({ warnings }: { warnings: ConversionWarning[] }): React.ReactElement | null {
  const { t } = useTranslation();
  if (warnings.length === 0) {
    return null;
  }
  return (
    <div className="flex flex-col gap-3">
      {warnings.map((warning, index) => (
        <Banner
          // The API gives no id, and two warnings can share a class and a message.
          key={`${index}-${warning.class}-${warning.detail ?? ""}`}
          tone={warning.class === "note" ? "info" : "warning"}
          title={t(`apps.install.warnings.class.${warning.class}`, { defaultValue: warning.class })}
          description={
            <div className="flex flex-col gap-2">
              <p>{warning.message}</p>
              {warning.command !== undefined ? (
                <CopyValue value={warning.command} label={t("apps.install.warnings.command")} />
              ) : null}
            </div>
          }
        />
      ))}
    </div>
  );
}
