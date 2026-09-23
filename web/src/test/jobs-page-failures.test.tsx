import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { JobDetailPage } from "@/routes/jobs/detail";
import { JobsPage } from "@/routes/jobs/index";

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

describe("Jobs pages load failures", () => {
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

  it("shows a jobs list error instead of the empty state when /jobs fails", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/jobs") {
        return Promise.resolve({ error: { message: "jobs unavailable" }, response: { ok: false } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <JobsPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("jobs unavailable")).toBeInTheDocument();
    expect(screen.queryByText("No jobs are running")).not.toBeInTheDocument();
  });

  it("shows a job detail error instead of the not-found state when the job request fails", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/jobs/{jobId}") {
        return Promise.resolve({ error: { message: "job unavailable" }, response: { ok: false } });
      }
      if (path === "/jobs/{jobId}/log") {
        return Promise.resolve({ data: "", response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter initialEntries={["/jobs/job-1"]}>
        <Routes>
          <Route path="/jobs/:jobId" element={<JobDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    expect(await screen.findByText("job unavailable")).toBeInTheDocument();
    expect(screen.queryByText("Job not found")).not.toBeInTheDocument();
  });
});
