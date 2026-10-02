import type { components } from "@/lib/api/client";

type CatalogSourceKind = components["schemas"]["CatalogSourceKind"];

export function sourceNoteKey(kind: CatalogSourceKind, signed: boolean): string {
  if (kind === "curated") {
    return "apps.catalogDetail.source.curated";
  }
  return signed ? "apps.catalogDetail.source.userAddedSigned" : "apps.catalogDetail.source.userAddedUnsigned";
}
