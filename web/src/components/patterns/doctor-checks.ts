import type { StatusTone } from "@/components/patterns/status-badge";
import type { components } from "@/lib/api/client";

type DoctorCheck = components["schemas"]["DoctorCheck"];

export function doctorStatusTone(status: DoctorCheck["status"]): StatusTone {
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

export function isStorageDependencyCheck(check: DoctorCheck): boolean {
  return check.id.startsWith("pkg_") && check.status === "fail";
}

export function isDockerWarningOnly(check: DoctorCheck): boolean {
  return (check.id === "docker" || check.id === "docker_compose") && check.status === "warn";
}

export function doctorBlocksProgress(checks: DoctorCheck[]): boolean {
  return checks.some((check) => isStorageDependencyCheck(check));
}
