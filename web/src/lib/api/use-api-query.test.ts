import { act, renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import type { ClientResult } from "@/lib/api/request";
import { useApiQuery } from "@/lib/api/use-api-query";

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (reason?: unknown) => void;
} {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe("useApiQuery", () => {
  it("treats a result error as failure, not empty data", async () => {
    const queryFn = vi.fn(
      async (): Promise<ClientResult<{ value: string }>> => ({
        error: { code: "pool_unavailable", message: "pool unavailable" },
      }),
    );

    const { result } = renderHook(() =>
      useApiQuery({
        queryKey: "pool",
        queryFn,
      }),
    );

    await waitFor(() => {
      expect(result.current.loading).toBe(false);
    });

    expect(result.current.error).toBe("pool unavailable");
    expect(result.current.data).toBeNull();
  });

  it("ignores abort and leaves state unchanged", async () => {
    const queryFn = vi.fn(async (signal: AbortSignal): Promise<ClientResult<{ value: string }>> => {
      await new Promise((resolve) => {
        signal.addEventListener("abort", () => resolve(undefined));
      });
      throw new DOMException("Aborted", "AbortError");
    });

    const { result, unmount } = renderHook(() =>
      useApiQuery({
        queryKey: "aborted",
        queryFn,
      }),
    );

    await waitFor(() => {
      expect(queryFn).toHaveBeenCalled();
    });
    const signal = queryFn.mock.calls[0]?.[0];
    unmount();

    expect(signal?.aborted).toBe(true);
    expect(result.current.error).toBeNull();
    expect(result.current.data).toBeNull();
  });

  it("does not report loading during a refresh that already has data", async () => {
    const pending = deferred<ClientResult<{ value: string }>>();
    let call = 0;
    const queryFn = vi.fn(async (): Promise<ClientResult<{ value: string }>> => {
      call += 1;
      if (call === 1) {
        return { data: { value: "ok" } };
      }
      return pending.promise;
    });

    const { result } = renderHook(() =>
      useApiQuery({
        queryKey: "refresh-loading",
        queryFn,
      }),
    );

    await waitFor(() => {
      expect(result.current.data).toEqual({ value: "ok" });
    });

    await act(async () => {
      void result.current.refresh();
    });

    await waitFor(() => {
      expect(result.current.refreshing).toBe(true);
    });
    expect(result.current.loading).toBe(false);
    expect(result.current.data).toEqual({ value: "ok" });

    await act(async () => {
      pending.resolve({ data: { value: "next" } });
    });

    await waitFor(() => {
      expect(result.current.data).toEqual({ value: "next" });
    });
    expect(result.current.loading).toBe(false);
    expect(result.current.refreshing).toBe(false);
  });

  it("keeps the last good value when a refresh fails", async () => {
    let shouldFail = false;
    const queryFn = vi.fn(async (): Promise<ClientResult<{ value: string }>> => {
      if (shouldFail) {
        return { error: { code: "refresh_failed", message: "refresh failed" } };
      }
      return { data: { value: "ok" } };
    });

    const { result } = renderHook(() =>
      useApiQuery({
        queryKey: "refresh",
        queryFn,
      }),
    );

    await waitFor(() => {
      expect(result.current.data).toEqual({ value: "ok" });
    });

    shouldFail = true;
    await act(async () => {
      await result.current.refresh();
    });

    expect(result.current.data).toEqual({ value: "ok" });
    expect(result.current.error).toBe("refresh failed");
  });

  it("discards a stale response when a newer fetch for the same key completes first", async () => {
    const first = deferred<ClientResult<{ value: string }>>();
    const second = deferred<ClientResult<{ value: string }>>();
    let call = 0;
    const queryFn = vi.fn(async (): Promise<ClientResult<{ value: string }>> => {
      call += 1;
      if (call === 1) {
        return first.promise;
      }
      return second.promise;
    });

    const { result } = renderHook(() =>
      useApiQuery({
        queryKey: "same-key",
        queryFn,
      }),
    );

    await waitFor(() => {
      expect(queryFn).toHaveBeenCalledTimes(1);
    });

    await act(async () => {
      void result.current.refresh();
    });

    await waitFor(() => {
      expect(queryFn).toHaveBeenCalledTimes(2);
    });

    await act(async () => {
      second.resolve({ data: { value: "new" } });
    });

    await waitFor(() => {
      expect(result.current.data).toEqual({ value: "new" });
    });

    await act(async () => {
      first.resolve({ data: { value: "stale" } });
    });

    expect(result.current.data).toEqual({ value: "new" });
  });

  it("discards a stale response when the query key changes", async () => {
    const first = deferred<ClientResult<{ value: string }>>();
    const second = deferred<ClientResult<{ value: string }>>();
    const queryFn = vi.fn(async (): Promise<ClientResult<{ value: string }>> => {
      if (queryFn.mock.calls.length === 1) {
        return first.promise;
      }
      return second.promise;
    });

    const { result, rerender } = renderHook(
      ({ key }: { key: string }) =>
        useApiQuery({
          queryKey: key,
          queryFn,
        }),
      { initialProps: { key: "a" } },
    );

    await waitFor(() => {
      expect(queryFn).toHaveBeenCalledTimes(1);
    });

    rerender({ key: "b" });
    await waitFor(() => {
      expect(queryFn).toHaveBeenCalledTimes(2);
    });

    await act(async () => {
      second.resolve({ data: { value: "new" } });
    });

    await waitFor(() => {
      expect(result.current.data).toEqual({ value: "new" });
    });

    await act(async () => {
      first.resolve({ data: { value: "stale" } });
    });

    expect(result.current.data).toEqual({ value: "new" });
  });

  it("surfaces rejected requests as errors", async () => {
    const queryFn = vi.fn(async (): Promise<ClientResult<{ value: string }>> => {
      throw new Error("network down");
    });

    const { result } = renderHook(() =>
      useApiQuery({
        queryKey: "reject",
        queryFn,
        fallbackError: "load failed",
      }),
    );

    await waitFor(() => {
      expect(result.current.loading).toBe(false);
    });

    expect(result.current.error).toBe("network down");
    expect(result.current.data).toBeNull();
  });
});
