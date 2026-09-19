import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { SystemDataContext, type SystemDataValue } from "@/hooks/system-data-context";
import { hoservaClient, type components } from "@/lib/api/client";

type SystemStatus = components["schemas"]["SystemStatus"];
type PoolStatus = components["schemas"]["PoolStatus"];
type Job = components["schemas"]["Job"];
type DoctorReport = components["schemas"]["DoctorReport"];

const POLL_MS = 30_000;

async function fetchSystemData(): Promise<{
  status?: SystemStatus | null;
  pool?: PoolStatus | null;
  jobs?: Job[];
  doctor?: DoctorReport | null;
  error: string | null;
}> {
  const [statusResult, poolResult, jobsResult, doctorResult] = await Promise.all([
    hoservaClient.GET("/status"),
    hoservaClient.GET("/pool"),
    hoservaClient.GET("/jobs", { params: { query: { limit: 20 } } }),
    hoservaClient.GET("/doctor"),
  ]);

  const error =
    statusResult.error?.message ??
    poolResult.error?.message ??
    jobsResult.error?.message ??
    doctorResult.error?.message ??
    null;

  return {
    status: statusResult.error ? undefined : (statusResult.data ?? null),
    pool: poolResult.error ? undefined : (poolResult.data ?? null),
    jobs: jobsResult.error ? undefined : (jobsResult.data?.jobs ?? []),
    doctor: doctorResult.error ? undefined : (doctorResult.data ?? null),
    error,
  };
}

export function SystemDataProvider({ children }: { children: ReactNode }): React.ReactElement {
  const { t } = useTranslation();
  const [status, setStatus] = useState<SystemStatus | null>(null);
  const [pool, setPool] = useState<PoolStatus | null>(null);
  const [jobs, setJobs] = useState<Job[]>([]);
  const [doctor, setDoctor] = useState<DoctorReport | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const applyFetch = useCallback(
    (next: Awaited<ReturnType<typeof fetchSystemData>>): void => {
      if (next.status !== undefined) setStatus(next.status);
      if (next.pool !== undefined) setPool(next.pool);
      if (next.jobs !== undefined) setJobs(next.jobs);
      if (next.doctor !== undefined) setDoctor(next.doctor);
      setError(next.error);
      setLoading(false);
    },
    [],
  );

  const refresh = useCallback(async (): Promise<void> => {
    try {
      applyFetch(await fetchSystemData());
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : t("systemData.loadFailed"));
      setLoading(false);
    }
  }, [applyFetch, t]);

  useEffect(() => {
    let cancelled = false;

    void fetchSystemData()
      .then((next) => {
        if (!cancelled) applyFetch(next);
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : t("systemData.loadFailed"));
          setLoading(false);
        }
      });

    const interval = window.setInterval(() => {
      void refresh();
    }, POLL_MS);

    return () => {
      cancelled = true;
      window.clearInterval(interval);
    };
  }, [applyFetch, refresh, t]);

  const value = useMemo<SystemDataValue>(
    () => ({ status, pool, jobs, doctor, loading, error, refresh }),
    [status, pool, jobs, doctor, loading, error, refresh],
  );

  return <SystemDataContext.Provider value={value}>{children}</SystemDataContext.Provider>;
}
