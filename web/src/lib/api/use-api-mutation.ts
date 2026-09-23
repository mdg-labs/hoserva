import { useCallback, useEffect, useRef, useState } from "react";

import { isAbortError, parseClientResult, type ClientResult } from "@/lib/api/request";

export type MutationResult<T> =
  | { ok: true; data: T | undefined }
  | { ok: false; error: string; aborted?: boolean };

export type UseApiMutationOptions<TArgs, TResult> = {
  mutationFn: (args: TArgs) => Promise<ClientResult<TResult>>;
  fallbackError?: string;
};

export type UseApiMutationResult<TArgs, TResult> = {
  mutate: (args: TArgs) => Promise<MutationResult<TResult>>;
  pending: boolean;
  error: string | null;
};

export function useApiMutation<TArgs, TResult>({
  mutationFn,
  fallbackError,
}: UseApiMutationOptions<TArgs, TResult>): UseApiMutationResult<TArgs, TResult> {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const mutationFnRef = useRef(mutationFn);

  useEffect(() => {
    mutationFnRef.current = mutationFn;
  }, [mutationFn]);

  const mutate = useCallback(
    async (args: TArgs): Promise<MutationResult<TResult>> => {
      setPending(true);
      setError(null);
      try {
        const result = await mutationFnRef.current(args);
        const parsed = parseClientResult(result, fallbackError);
        if (parsed.error) {
          setError(parsed.error);
          return { ok: false, error: parsed.error };
        }
        return { ok: true, data: parsed.data };
      } catch (err: unknown) {
        if (isAbortError(err)) {
          return { ok: false, error: "", aborted: true };
        }
        const message = err instanceof Error ? err.message : (fallbackError ?? String(err));
        setError(message);
        return { ok: false, error: message };
      } finally {
        setPending(false);
      }
    },
    [fallbackError],
  );

  return { mutate, pending, error };
}
