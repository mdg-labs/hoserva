import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { CopyValue } from "@/components/patterns/copy-value";
import { doctorStatusLabel, doctorStatusTone } from "@/components/patterns/doctor-checks";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Frame, FramePanel } from "@/components/ui/frame";
import type { components } from "@/lib/api/client";

type DoctorCheck = components["schemas"]["DoctorCheck"];

export function StackedChecks({
  checks,
  footer,
}: {
  checks: DoctorCheck[];
  footer?: ReactNode;
}): React.ReactElement {
  const { t } = useTranslation();
  return (
    <Frame>
      {checks.map((check) => (
        <FramePanel key={check.id} className="flex flex-col gap-3">
          <div className="flex flex-wrap items-start justify-between gap-2">
            <div>
              <p className="font-medium">{check.name}</p>
              <p className="text-muted-foreground text-sm">{check.message}</p>
            </div>
            <StatusBadge tone={doctorStatusTone(check.status)}>{doctorStatusLabel(check.status, t)}</StatusBadge>
          </div>
          {check.remediation ? <CopyValue value={check.remediation} label={check.name} /> : null}
        </FramePanel>
      ))}
      {footer ? <FramePanel>{footer}</FramePanel> : null}
    </Frame>
  );
}
