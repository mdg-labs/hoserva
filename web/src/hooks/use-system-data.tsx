import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";

import { SystemDataContext, type SystemDataValue } from "@/hooks/system-data-context";
import { hoservaClient, type components } from "@/lib/api/client";

type SystemStatus = components["schemas"]["SystemStatus"];
type PoolStatus = components["schemas"]["PoolStatus"];
type Job = components["schemas"]["Job"];
type DoctorReport = components["schemas"]["DoctorReport"];

const POLL_MS = 30_000;

async function fetchSystemData(): Promise<{
  status: SystemStatus | null;
  pool: PoolStatus | null;
  jobs: Job[];
  doctor: DoctorReport | null;
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
    status: statusResult.data ?? null,
    pool: poolResult.data ?? null,
    jobs: jobsResult.data?.jobs ?? [],
    doctor: doctorResult.data ?? null,
    error,
  };
}

export function SystemDataProvider({ children }: { children: ReactNode }): React.ReactElement {
  const [status, setStatus] = useState<SystemStatus | null>(null);
  const [pool, setPool] = useState<PoolStatus | null>(null);
  const [jobs, setJobs] = useState<Job[]>([]);
  const [doctor, setDoctor] = useState<DoctorReport | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async (): Promise<void> => {
    const next = await fetchSystemData();
    setStatus(next.status);
    setPool(next.pool);
    setJobs(next.jobs);
    setDoctor(next.doctor);
    setError(next.error);
    setLoading(false);
  }, []);

  useEffect(() => {
    let cancelled = false;

    void fetchSystemData()
      .then((next) => {
        if (cancelled) return;
        setStatus(next.status);
        setPool(next.pool);
        setJobs(next.jobs);
        setDoctor(next.doctor);
        setError(next.error);
        setLoading(false);
      })
      .catch(() => {
        if (!cancelled) {
          setError("Failed to load system data");
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
  }, [refresh]);

  const value = useMemo<SystemDataValue>(
    () => ({ status, pool, jobs, doctor, loading, error, refresh }),
    [status, pool, jobs, doctor, loading, error, refresh],
  );

  return <SystemDataContext.Provider value={value}>{children}</SystemDataContext.Provider>;
}
