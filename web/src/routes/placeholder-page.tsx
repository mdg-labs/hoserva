// A route body for a section this issue doesn't build yet (#21 is the app
// shell and shared patterns; the page inventory in doc 03 arrives with its
// own issue). Every string still comes from the i18n catalog.
import { useTranslation } from "react-i18next";

export function PlaceholderPage({ titleKey }: { titleKey: string }): React.ReactElement {
  const { t } = useTranslation();

  return (
    <div className="flex flex-col gap-2">
      <h1 className="text-2xl font-semibold font-heading">{t(titleKey)}</h1>
      <p className="text-muted-foreground">{t("placeholderPage.description")}</p>
    </div>
  );
}
