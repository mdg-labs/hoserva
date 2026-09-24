import type { components } from "@/lib/api/client";
import { isApiError } from "@/lib/api/errors";

type ShareCacheMode = components["schemas"]["ShareCacheMode"];

// shareRelocationDirection is the one rule both the share detail page and
// the cache page use to offer "change mode and relocate": which tier a
// share's existing files move to for a from → to cache-mode change, or
// null when the change needs no relocation.
export function shareRelocationDirection(from: ShareCacheMode, to: ShareCacheMode): "cache" | "array" | null {
  if (from === to) {
    return null;
  }
  if (to === "array-only") {
    return "array";
  }
  if (from === "array-only") {
    return "cache";
  }
  if (from === "cache-then-move" && to === "cache-only") {
    return "cache";
  }
  return null;
}

export function shareMutationError(err: unknown, t: (key: string) => string): string {
  if (isApiError(err) && err.code === "maintenance_mode") {
    return t("shares.errors.maintenanceMode");
  }
  if (isApiError(err)) {
    return err.message;
  }
  if (err instanceof Error) {
    return err.message;
  }
  return String(err);
}
