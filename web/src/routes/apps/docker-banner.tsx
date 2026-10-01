import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { CopyValue } from "@/components/patterns/copy-value";
import type { components } from "@/lib/api/client";

type DoctorCheck = components["schemas"]["DoctorCheck"];

// The install commands come from the daemon's own doctor check, never from
// text written here.
function isDockerCheck(check: DoctorCheck): boolean {
  return (check.id === "docker" || check.id === "docker_compose") && check.status !== "pass";
}

export function DockerBanner({ message, checks }: { message?: string; checks: DoctorCheck[] }): React.ReactElement {
  const { t } = useTranslation();
  const remediations = checks.filter((check) => isDockerCheck(check) && check.remediation);
  return (
    <Banner
      tone="warning"
      title={t("apps.docker.title")}
      description={
        <div className="flex flex-col gap-3">
          <p>{message ?? t("apps.docker.description")}</p>
          {remediations.map((check) => (
            <div key={check.id} className="flex flex-col gap-1">
              <span className="font-medium">{check.name}</span>
              <CopyValue value={check.remediation ?? ""} label={check.name} />
            </div>
          ))}
        </div>
      }
    />
  );
}
