import type { ApiError } from "@/lib/api/errors";

export type ClientResult<T> = {
  data?: T;
  error?: ApiError;
};

export function isAbortError(error: unknown, signal?: AbortSignal): boolean {
  if (signal?.aborted) {
    return true;
  }
  if (error instanceof DOMException && error.name === "AbortError") {
    return true;
  }
  if (error instanceof Error && error.name === "AbortError") {
    return true;
  }
  return false;
}

export function apiErrorMessage(error: ApiError | undefined, fallback?: string): string {
  if (error?.message) {
    return error.message;
  }
  return fallback ?? "Request failed";
}

export function parseClientResult<T>(
  result: ClientResult<T>,
  fallback?: string,
): { data: T | undefined; error: string | null } {
  if (result.error) {
    return { data: undefined, error: apiErrorMessage(result.error, fallback) };
  }
  return { data: result.data, error: null };
}
