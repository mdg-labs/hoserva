import { useCallback, useState } from "react";

import type { components } from "@/lib/api/client";
import { getShareRelocationPrecheck, postAppStop } from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";

export type RelocationPrecheck = components["schemas"]["ShareRelocationPrecheck"];

export type RelocationPrecheckState = {
  enabled: boolean;
  data: RelocationPrecheck | null;
  error: string | null;
  loading: boolean;
  retry: () => void;
  stopContainer: (id: string) => Promise<void>;
  stoppingId: string | null;
  stopError: string | null;
  // True while a container is being stopped, so the dialog around it stays open.
  busy: boolean;
  // True only for a settled, successful answer that lists no active container: an
  // unanswered, failed or stale check never clears a relocation.
  canRelocate: boolean;
};

// useRelocationPrecheck asks the daemon what a relocation of shareName would collide with
// (doc 09 §2) and offers to stop the active containers it lists. It is disabled until the
// share is known, which is how the dialogs that own it keep it from reading the share's
// disks while closed. A refusal (a stopped array, an unfinished migration) comes back as
// the query's error, never as an empty answer.
export function useRelocationPrecheck(shareName: string | null): RelocationPrecheckState {
  const enabled = shareName !== null;
  const query = useApiQuery<RelocationPrecheck>({
    queryKey: ["relocation-precheck", shareName],
    queryFn: (signal) => getShareRelocationPrecheck(shareName ?? "", signal),
    enabled,
  });
  const stopMutation = useApiMutation<string, components["schemas"]["App"]>({
    mutationFn: postAppStop,
  });
  const [stoppingId, setStoppingId] = useState<string | null>(null);
  const [stopError, setStopError] = useState<string | null>(null);
  const { refresh } = query;
  const { mutate } = stopMutation;

  const stopContainer = useCallback(
    async (id: string): Promise<void> => {
      setStopError(null);
      setStoppingId(id);
      try {
        const result = await mutate(id);
        if (!result.ok) {
          if (!result.aborted) setStopError(result.error);
          return;
        }
        await refresh();
      } finally {
        setStoppingId(null);
      }
    },
    [mutate, refresh],
  );

  const retry = useCallback((): void => {
    void refresh();
  }, [refresh]);

  const data = enabled ? query.data : null;
  const canRelocate =
    enabled &&
    data !== null &&
    query.error === null &&
    !query.refreshing &&
    stoppingId === null &&
    !data.containers.some((container) => container.active);

  return {
    enabled,
    data,
    error: enabled ? query.error : null,
    loading: enabled && query.loading,
    retry,
    stopContainer,
    stoppingId,
    stopError,
    busy: stoppingId !== null,
    canRelocate,
  };
}
