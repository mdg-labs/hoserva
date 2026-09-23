import { act, renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import type { ClientResult } from "@/lib/api/request";
import { useApiMutation } from "@/lib/api/use-api-mutation";

describe("useApiMutation", () => {
  it("reports failure to the caller without treating it as success", async () => {
    const mutationFn = vi.fn(async (): Promise<ClientResult<{ id: string }>> => ({
      error: { code: "sync_refused", message: "sync refused" },
    }));

    const { result } = renderHook(() =>
      useApiMutation({
        mutationFn,
      }),
    );

    let mutationResult: Awaited<ReturnType<typeof result.current.mutate>> | undefined;
    await act(async () => {
      mutationResult = await result.current.mutate(undefined);
    });

    expect(mutationResult).toEqual({ ok: false, error: "sync refused" });
    expect(result.current.error).toBe("sync refused");
  });

  it("returns success only after a successful mutation", async () => {
    const mutationFn = vi.fn(async (): Promise<ClientResult<{ id: string }>> => ({
      data: { id: "job-1" },
    }));

    const { result } = renderHook(() =>
      useApiMutation({
        mutationFn,
      }),
    );

    let mutationResult: Awaited<ReturnType<typeof result.current.mutate>> | undefined;
    await act(async () => {
      mutationResult = await result.current.mutate(undefined);
    });

    expect(mutationResult).toEqual({ ok: true, data: { id: "job-1" } });
    expect(result.current.error).toBeNull();
  });

  it("treats abort as neither success nor a surfaced error", async () => {
    const mutationFn = vi.fn(async (): Promise<ClientResult<{ id: string }>> => {
      throw new DOMException("Aborted", "AbortError");
    });

    const { result } = renderHook(() =>
      useApiMutation({
        mutationFn,
      }),
    );

    let mutationResult: Awaited<ReturnType<typeof result.current.mutate>> | undefined;
    await act(async () => {
      mutationResult = await result.current.mutate(undefined);
    });

    expect(mutationResult).toEqual({ ok: false, error: "", aborted: true });
    expect(result.current.error).toBeNull();
  });

  it("clears pending after the mutation settles", async () => {
    const mutationFn = vi.fn(
      async (): Promise<ClientResult<undefined>> =>
        new Promise((resolve) => {
          setTimeout(() => resolve({ data: undefined }), 10);
        }),
    );

    const { result } = renderHook(() =>
      useApiMutation({
        mutationFn,
      }),
    );

    await act(async () => {
      await result.current.mutate(undefined);
    });

    await waitFor(() => {
      expect(result.current.pending).toBe(false);
    });
  });
});
