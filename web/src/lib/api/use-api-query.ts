import { useCallback, useEffect, useRef, useState } from "react";

import { isAbortError, parseClientResult, type ClientResult } from "@/lib/api/request";

export type UseApiQueryOptions<T> = {
  queryKey: unknown;
  queryFn: (signal: AbortSignal) => Promise<ClientResult<T>>;
  enabled?: boolean;
  pollIntervalMs?: number;
  fallbackError?: string;
};

export type UseApiQueryResult<T> = {
  data: T | null;
  error: string | null;
  loading: boolean;
  refreshing: boolean;
  refresh: () => Promise<void>;
};

function queryKeyToken(queryKey: unknown): string {
  return JSON.stringify(queryKey);
}

export function useApiQuery<T>({
  queryKey,
  queryFn,
  enabled = true,
  pollIntervalMs,
  fallbackError,
}: UseApiQueryOptions<T>): UseApiQueryResult<T> {
  const keyToken = queryKeyToken(queryKey);
  const [trackedKey, setTrackedKey] = useState(keyToken);
  const [epoch, setEpoch] = useState(0);
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [fetching, setFetching] = useState(enabled);
  const queryFnRef = useRef(queryFn);
  const activeControllerRef = useRef<AbortController | null>(null);
  const fetchGenerationRef = useRef(0);
  const latestEpochRef = useRef(epoch);

  useEffect(() => {
    queryFnRef.current = queryFn;
  }, [queryFn]);

  useEffect(() => {
    latestEpochRef.current = epoch;
  }, [epoch]);

  if (trackedKey !== keyToken) {
    setTrackedKey(keyToken);
    setData(null);
    setError(null);
    setFetching(true);
    setEpoch((value) => value + 1);
  }

  const runFetch = useCallback(
    async (signal: AbortSignal, keyEpoch: number, fetchGeneration: number): Promise<void> => {
      try {
        const result = await queryFnRef.current(signal);
        if (
          signal.aborted ||
          fetchGeneration !== fetchGenerationRef.current ||
          keyEpoch !== latestEpochRef.current
        ) {
          return;
        }

        const parsed = parseClientResult(result, fallbackError);
        if (parsed.error) {
          setError(parsed.error);
        } else {
          setData(parsed.data ?? null);
          setError(null);
        }
      } catch (err: unknown) {
        if (
          isAbortError(err, signal) ||
          fetchGeneration !== fetchGenerationRef.current ||
          keyEpoch !== latestEpochRef.current
        ) {
          return;
        }
        const message = err instanceof Error ? err.message : (fallbackError ?? String(err));
        setError(message);
      } finally {
        if (
          !signal.aborted &&
          fetchGeneration === fetchGenerationRef.current &&
          keyEpoch === latestEpochRef.current
        ) {
          setFetching(false);
        }
      }
    },
    [fallbackError],
  );

  const startFetch = useCallback(
    async (keyEpoch: number): Promise<void> => {
      activeControllerRef.current?.abort();
      const controller = new AbortController();
      activeControllerRef.current = controller;
      const generation = ++fetchGenerationRef.current;
      setFetching(true);
      await runFetch(controller.signal, keyEpoch, generation);
    },
    [runFetch],
  );

  const refresh = useCallback(async (): Promise<void> => {
    await startFetch(epoch);
  }, [epoch, startFetch]);

  useEffect(() => {
    if (!enabled) {
      return;
    }

    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) {
        void startFetch(epoch);
      }
    });

    let interval: number | undefined;
    if (pollIntervalMs && pollIntervalMs > 0) {
      interval = window.setInterval(() => {
        void startFetch(epoch);
      }, pollIntervalMs);
    }

    return () => {
      cancelled = true;
      activeControllerRef.current?.abort();
      activeControllerRef.current = null;
      if (interval !== undefined) {
        window.clearInterval(interval);
      }
    };
  }, [enabled, epoch, startFetch, pollIntervalMs]);

  const refreshing = enabled && fetching;
  return { data, error, loading: refreshing && data === null, refreshing, refresh };
}
