import type { ReactNode } from "react";

import { CopyValue } from "@/components/patterns/copy-value";
import { StatusBadge, type StatusTone } from "@/components/patterns/status-badge";
import { Frame, FramePanel } from "@/components/ui/frame";
import type { components } from "@/lib/api/client";

type DoctorCheck = components["schemas"]["DoctorCheck"];

function doctorStatusTone(status: DoctorCheck["status"]): StatusTone {
  switch (status) {
    case "pass":
      return "success";
    case "warn":
      return "warning";
    case "fail":
      return "error";
    default:
      return "outline";
  }
}

export function StackedChecks({
  checks,
  footer,
}: {
  checks: DoctorCheck[];
  footer?: ReactNode;
}): React.ReactElement {
  return (
    <Frame>
      {checks.map((check) => (
        <FramePanel key={check.id} className="flex flex-col gap-3">
          <div className="flex flex-wrap items-start justify-between gap-2">
            <div>
              <p className="font-medium">{check.name}</p>
              <p className="text-muted-foreground text-sm">{check.message}</p>
            </div>
            <StatusBadge tone={doctorStatusTone(check.status)}>{check.status}</StatusBadge>
          </div>
          {check.remediation ? <CopyValue value={check.remediation} label={check.name} /> : null}
        </FramePanel>
      ))}
      {footer ? <FramePanel>{footer}</FramePanel> : null}
    </Frame>
  );
}
