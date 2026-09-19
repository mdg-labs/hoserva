import { useContext } from "react";

import { SystemDataContext, type SystemDataValue } from "@/hooks/system-data-context";

export function useSystemData(): SystemDataValue {
  const value = useContext(SystemDataContext);
  if (!value) {
    throw new Error("useSystemData must be used within SystemDataProvider");
  }
  return value;
}
