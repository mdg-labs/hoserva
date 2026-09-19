import { createContext } from "react";

import type { components } from "@/lib/api/client";

type SystemStatus = components["schemas"]["SystemStatus"];
type PoolStatus = components["schemas"]["PoolStatus"];
type Job = components["schemas"]["Job"];
type DoctorReport = components["schemas"]["DoctorReport"];

export interface SystemDataValue {
  status: SystemStatus | null;
  pool: PoolStatus | null;
  jobs: Job[];
  doctor: DoctorReport | null;
  loading: boolean;
  error: string | null;
  refresh: () => Promise<void>;
}

export const SystemDataContext = createContext<SystemDataValue | null>(null);
