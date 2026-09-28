import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { JobsPage } from "@/routes/jobs/index";
import type { components } from "@/lib/api/client";

type Job = components["schemas"]["Job"];
type GetOptions = { params?: { query?: { status?: string; class?: string; limit?: number } } };

const mockGet = vi.fn();
const mockPost = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
  },
}));

vi.mock("@/lib/api/auth-context", () => ({
  useAuth: () => ({
    phase: "authenticated",
    user: { id: "1", username: "admin", role: "admin", totpEnrolled: false },
    adminExists: true,
    refresh: vi.fn(),
    acceptSession: vi.fn(),
  }),
}));

function job(overrides: Partial<Job>): Job {
  return {
    id: "job-1",
    type: "sync",
    class: "parity",
    status: "succeeded",
    progress: null,
    resumable: false,
    cancellable: false,
    createdAt: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

async function selectOption(comboboxName: string, optionName: string): Promise<void> {
  fireEvent.click(screen.getByRole("combobox", { name: comboboxName }));
  const option = await screen.findByRole("option", { name: optionName });
  fireEvent.pointerDown(option, { pointerType: "mouse" });
  fireEvent.pointerUp(option, { pointerType: "mouse" });
  fireEvent.click(option);
}

describe("Jobs page filters", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    window.matchMedia = vi.fn().mockImplementation((query: string) => ({
      addEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
      matches: false,
      media: query,
      onchange: null,
      removeEventListener: vi.fn(),
    })) as unknown as typeof window.matchMedia;
  });

  it("sends the selected status filter to listJobs and sends none for 'all'", async () => {
    mockGet.mockImplementation((path: string) =>
      Promise.resolve(
        path === "/jobs"
          ? { data: { jobs: [job({ id: "job-mover", type: "mover" })] }, response: { ok: true } }
          : { data: null, response: { ok: false } },
      ),
    );

    render(
      <MemoryRouter>
        <JobsPage />
      </MemoryRouter>,
    );
    await screen.findByRole("link", { name: "mover" });

    const initialQuery = (mockGet.mock.calls[0]?.[1] as GetOptions).params?.query;
    expect(initialQuery?.status).toBeUndefined();
    expect(initialQuery?.class).toBeUndefined();

    mockGet.mockClear();
    await selectOption("Status", "Failed");
    await screen.findByRole("link", { name: "mover" });

    const filteredQuery = (mockGet.mock.calls[0]?.[1] as GetOptions).params?.query;
    expect(filteredQuery?.status).toBe("failed");
  });

  it("shows a job matching the status filter that was not in the unfiltered page", async () => {
    mockGet.mockImplementation((path: string, options: GetOptions) => {
      if (path !== "/jobs") return Promise.resolve({ data: null, response: { ok: false } });
      if (options?.params?.query?.status === "failed") {
        return Promise.resolve({
          data: { jobs: [job({ id: "job-failed", type: "scrub", status: "failed" })] },
          response: { ok: true },
        });
      }
      return Promise.resolve({
        data: { jobs: [job({ id: "job-ok", type: "mover", status: "succeeded" })] },
        response: { ok: true },
      });
    });

    render(
      <MemoryRouter>
        <JobsPage />
      </MemoryRouter>,
    );
    await screen.findByRole("link", { name: "mover" });

    await selectOption("Status", "Failed");

    await screen.findByRole("link", { name: "scrub" });
    expect(screen.queryByRole("link", { name: "mover" })).not.toBeInTheDocument();
  });

  it("keeps the error state instead of an empty list when a filtered re-fetch fails", async () => {
    mockGet.mockImplementation((path: string, options: GetOptions) => {
      if (path !== "/jobs") return Promise.resolve({ data: null, response: { ok: false } });
      if (options?.params?.query?.status === "failed") {
        return Promise.resolve({ error: { message: "jobs unavailable" }, response: { ok: false } });
      }
      return Promise.resolve({
        data: { jobs: [job({ id: "job-ok", type: "mover", status: "succeeded" })] },
        response: { ok: true },
      });
    });

    render(
      <MemoryRouter>
        <JobsPage />
      </MemoryRouter>,
    );
    await screen.findByRole("link", { name: "mover" });

    await selectOption("Status", "Failed");

    expect(await screen.findByText("jobs unavailable")).toBeInTheDocument();
    expect(screen.queryByText("No jobs are running")).not.toBeInTheDocument();
  });
});
