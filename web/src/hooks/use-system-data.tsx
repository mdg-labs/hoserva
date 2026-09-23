import { useCallback, useMemo, type ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { SystemDataContext, type SystemDataValue } from "@/hooks/system-data-context";
import { getDoctor, getJobs, getPool, getStatus } from "@/lib/api/operations";
import { useApiQuery } from "@/lib/api/use-api-query";
import type { components } from "@/lib/api/client";

type SystemStatus = components["schemas"]["SystemStatus"];
type PoolStatus = components["schemas"]["PoolStatus"];
type Job = components["schemas"]["Job"];
type DoctorReport = components["schemas"]["DoctorReport"];

const POLL_MS = 30_000;

export function SystemDataProvider({ children }: { children: ReactNode }): React.ReactElement {
  const { t } = useTranslation();
  const loadFailed = t("systemData.loadFailed");

  const statusQuery = useApiQuery<SystemStatus>({
    queryKey: "system-status",
    queryFn: (signal) => getStatus(signal),
    pollIntervalMs: POLL_MS,
    fallbackError: loadFailed,
  });
  const poolQuery = useApiQuery<PoolStatus>({
    queryKey: "system-pool",
    queryFn: (signal) => getPool(signal),
    pollIntervalMs: POLL_MS,
    fallbackError: loadFailed,
  });
  const jobsQuery = useApiQuery<{ jobs: Job[] }>({
    queryKey: "system-jobs",
    queryFn: (signal) => getJobs({ limit: 20 }, signal),
    pollIntervalMs: POLL_MS,
    fallbackError: loadFailed,
  });
  const doctorQuery = useApiQuery<DoctorReport>({
    queryKey: "system-doctor",
    queryFn: (signal) => getDoctor(signal),
    pollIntervalMs: POLL_MS,
    fallbackError: loadFailed,
  });

  const refresh = useCallback(async (): Promise<void> => {
    await Promise.all([
      statusQuery.refresh(),
      poolQuery.refresh(),
      jobsQuery.refresh(),
      doctorQuery.refresh(),
    ]);
  }, [statusQuery, poolQuery, jobsQuery, doctorQuery]);

  const error =
    statusQuery.error ?? poolQuery.error ?? jobsQuery.error ?? doctorQuery.error ?? null;
  const loading =
    statusQuery.loading || poolQuery.loading || jobsQuery.loading || doctorQuery.loading;

  const value = useMemo<SystemDataValue>(
    () => ({
      status: statusQuery.data,
      pool: poolQuery.data,
      jobs: jobsQuery.data?.jobs ?? [],
      doctor: doctorQuery.data,
      loading,
      error,
      refresh,
    }),
    [
      statusQuery.data,
      poolQuery.data,
      jobsQuery.data,
      doctorQuery.data,
      loading,
      error,
      refresh,
    ],
  );

  return <SystemDataContext.Provider value={value}>{children}</SystemDataContext.Provider>;
}
