// The privilege summary of a template (doc 03 §5.3, doc 01 §7): one warning
// Alert per flag, in the order the API computed them. The API supplies the
// facts and their plain-language explanation; the kind only picks the short
// label, and nothing here decides what a template asks for.
import { AlertTriangle, ShieldCheck } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Frame, FramePanel } from "@/components/ui/frame";
import type { components } from "@/lib/api/client";

type TemplatePrivilege = components["schemas"]["TemplatePrivilege"];

// Keyed by the API's enum, so a new kind fails the type check here until it
// has a plain-language label, instead of showing its identifier.
const KIND_LABELS: Record<TemplatePrivilege["kind"], string> = {
  privileged: "privilegeSummary.kinds.privileged",
  host_network: "privilegeSummary.kinds.host_network",
  host_pid: "privilegeSummary.kinds.host_pid",
  host_cgroup: "privilegeSummary.kinds.host_cgroup",
  device_cgroup_rules: "privilegeSummary.kinds.device_cgroup_rules",
  added_capabilities: "privilegeSummary.kinds.added_capabilities",
  confinement_disabled: "privilegeSummary.kinds.confinement_disabled",
  group_add: "privilegeSummary.kinds.group_add",
  docker_socket: "privilegeSummary.kinds.docker_socket",
  host_path: "privilegeSummary.kinds.host_path",
  gpu_reservation: "privilegeSummary.kinds.gpu_reservation",
  container_runtime: "privilegeSummary.kinds.container_runtime",
};

export function PrivilegeSummary({
  privileges,
}: {
  privileges: TemplatePrivilege[];
}): React.ReactElement {
  const { t } = useTranslation();

  if (privileges.length === 0) {
    return (
      <Alert variant="success">
        <ShieldCheck aria-hidden="true" />
        <AlertTitle>{t("privilegeSummary.noneTitle")}</AlertTitle>
        <AlertDescription>{t("privilegeSummary.noneDescription")}</AlertDescription>
      </Alert>
    );
  }

  return (
    <Frame>
      {privileges.map((privilege) => (
        <FramePanel
          key={`${privilege.kind}:${privilege.service}:${privilege.detail ?? ""}`}
          className="p-2"
        >
          <Alert variant="warning">
            <AlertTriangle aria-hidden="true" />
            <AlertTitle>
              {t(KIND_LABELS[privilege.kind])}
            </AlertTitle>
            <AlertDescription>
              <p>{privilege.description}</p>
              {privilege.detail ? (
                <p className="mt-1 break-words font-mono text-xs">{privilege.detail}</p>
              ) : null}
              <p className="mt-1 text-xs">{t("privilegeSummary.service", { service: privilege.service })}</p>
            </AlertDescription>
          </Alert>
        </FramePanel>
      ))}
    </Frame>
  );
}
