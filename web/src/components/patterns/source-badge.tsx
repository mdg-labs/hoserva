// The source `status-badge` on a catalog template (doc 03 §5.2, §5.3): where
// it came from and whether that source's archive passed a signature check.
// Both come from the API as data and are only labelled here.
import { useTranslation } from "react-i18next";

import { StatusBadge, type StatusTone } from "@/components/patterns/status-badge";
import type { components } from "@/lib/api/client";

type CatalogSourceKind = components["schemas"]["CatalogSourceKind"];

function badgeKey(kind: CatalogSourceKind, signed: boolean): { key: string; tone: StatusTone } {
  if (kind === "curated") {
    return signed
      ? { key: "sourceBadge.curatedSigned", tone: "success" }
      : { key: "sourceBadge.curatedUnsigned", tone: "warning" };
  }
  return signed
    ? { key: "sourceBadge.userAddedSigned", tone: "info" }
    : { key: "sourceBadge.userAddedUnsigned", tone: "warning" };
}

export function SourceBadge({
  kind,
  signed,
}: {
  kind: CatalogSourceKind;
  signed: boolean;
}): React.ReactElement {
  const { t } = useTranslation();
  const { key, tone } = badgeKey(kind, signed);
  return <StatusBadge tone={tone}>{t(key)}</StatusBadge>;
}
