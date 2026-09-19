import { useTranslation } from "react-i18next";

import type { StatusTone } from "@/components/patterns/status-badge";
import type { components } from "@/lib/api/client";

type SystemStatus = components["schemas"]["SystemStatus"];
type DoctorReport = components["schemas"]["DoctorReport"];

export function arrayStatusTone(status: SystemStatus | null): StatusTone {
  if (!status) {
    return "outline";
  }
  if (status.maintenanceMode) {
    return "info";
  }
  if (status.arrayDegraded) {
    return "error";
  }
  if (status.parityBlocked) {
    return "warning";
  }
  if (!status.healthy) {
    return "warning";
  }
  return "success";
}

export function arrayStatusLabel(
  status: SystemStatus | null,
  t: ReturnType<typeof useTranslation>["t"],
): string {
  if (!status) {
    return t("topBar.arrayStatus.unknown");
  }
  if (status.maintenanceMode) {
    return t("topBar.arrayStatus.maintenance");
  }
  if (status.arrayDegraded) {
    return t("topBar.arrayStatus.degraded");
  }
  if (status.parityBlocked) {
    return t("topBar.arrayStatus.actionRequired");
  }
  if (!status.healthy) {
    return t("topBar.arrayStatus.actionRequired");
  }
  return t("topBar.arrayStatus.healthy");
}

export function parityFreshnessLabel(
  status: SystemStatus | null,
  doctor: DoctorReport | null,
  t: ReturnType<typeof useTranslation>["t"],
): { label: string; tone: StatusTone } {
  if (status?.parityBlocked) {
    return { label: t("topBar.parity.blocked"), tone: "error" };
  }

  const freshness = doctor?.checks.find((check) => check.id === "parity_freshness");
  if (freshness?.status === "warn") {
    return { label: freshness.message, tone: "warning" };
  }
  if (freshness?.status === "fail") {
    return { label: freshness.message, tone: "error" };
  }
  if (freshness?.message) {
    return { label: freshness.message, tone: "success" };
  }

  return { label: t("topBar.parity.unknown"), tone: "outline" };
}
