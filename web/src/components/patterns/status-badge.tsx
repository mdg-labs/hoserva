// `status-badge` (doc 03 "Shared patterns", `p-badge-5` through `p-badge-8`):
// every state in the product — array, disk, SMART, spin, job, container,
// VM, protocol — renders through this one mapping so a status never looks
// two different ways.
import type { ReactNode } from "react";

import { Badge } from "@/components/ui/badge";

export type StatusTone = "success" | "warning" | "error" | "info" | "outline";

const TONE_TO_VARIANT: Record<StatusTone, "success" | "warning" | "error" | "info" | "outline"> = {
  success: "success",
  warning: "warning",
  error: "error",
  info: "info",
  outline: "outline",
};

export function StatusBadge({ tone, children }: { tone: StatusTone; children: ReactNode }): React.ReactElement {
  return <Badge variant={TONE_TO_VARIANT[tone]}>{children}</Badge>;
}
