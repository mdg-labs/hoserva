// #418: every job status badge (jobs list, job detail, parity page, cache
// page) must render a catalog string for each JobStatus value, including
// `cancelled`, and a catalog fallback for anything else, instead of the raw
// API enum value.

import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { jobStatusLabel } from "@/components/patterns/job-status";
import i18n from "@/lib/i18n";
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

afterEach(() => {
  cleanup();
});

describe("jobStatusLabel (#418)", () => {
  it("returns a catalog label for every JobStatus value", () => {
    expect(jobStatusLabel("running", i18n.t)).toBe("Running");
    expect(jobStatusLabel("queued", i18n.t)).toBe("Queued");
    expect(jobStatusLabel("interrupted", i18n.t)).toBe("Interrupted");
    expect(jobStatusLabel("failed", i18n.t)).toBe("Failed");
    expect(jobStatusLabel("succeeded", i18n.t)).toBe("Succeeded");
    expect(jobStatusLabel("cancelled", i18n.t)).toBe("Cancelled");
  });

  it("returns the catalog fallback for an unrecognised status", () => {
    expect(jobStatusLabel("unexpected" as never, i18n.t)).toBe("Unknown");
  });
});

describe("jobs list page job status badges (#418)", () => {
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

  it("renders catalog labels for every job status, including cancelled, instead of the raw value", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/jobs") {
        return Promise.resolve({
          data: {
            jobs: [
              { id: "j-1", type: "sync", class: "parity", status: "running", resumable: false, cancellable: true, createdAt: "2026-01-01T00:00:00Z" },
              { id: "j-2", type: "sync", class: "parity", status: "queued", resumable: false, cancellable: true, createdAt: "2026-01-01T00:01:00Z" },
              { id: "j-3", type: "scrub", class: "parity", status: "interrupted", resumable: true, cancellable: false, createdAt: "2026-01-01T00:02:00Z" },
              { id: "j-4", type: "fix", class: "parity", status: "failed", resumable: false, cancellable: false, createdAt: "2026-01-01T00:03:00Z" },
              { id: "j-5", type: "mover", class: "array_write", status: "succeeded", resumable: true, cancellable: false, createdAt: "2026-01-01T00:04:00Z" },
              { id: "j-6", type: "mover", class: "array_write", status: "cancelled", resumable: false, cancellable: false, createdAt: "2026-01-01T00:05:00Z" },
            ],
          },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <JobsPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("Running")).toBeInTheDocument();
    expect(screen.getByText("Queued")).toBeInTheDocument();
    expect(screen.getByText("Interrupted")).toBeInTheDocument();
    expect(screen.getByText("Failed")).toBeInTheDocument();
    expect(screen.getByText("Succeeded")).toBeInTheDocument();
    expect(screen.getByText("Cancelled")).toBeInTheDocument();

    for (const raw of ["running", "queued", "interrupted", "failed", "succeeded", "cancelled"]) {
      expect(screen.queryByText(raw)).not.toBeInTheDocument();
    }
  });
});

describe("job detail page job status badge (#418)", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
  });

  it("renders the catalog label, not the raw status, for a cancelled job", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/jobs/{jobId}") {
        return Promise.resolve({
          data: {
            id: "job-1",
            type: "sync",
            class: "parity",
            status: "cancelled",
            resumable: false,
            cancellable: false,
            createdAt: "2026-01-01T00:00:00Z",
          },
          response: { ok: true },
        });
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

    expect(await screen.findByText("Cancelled")).toBeInTheDocument();
    expect(screen.queryByText("cancelled")).not.toBeInTheDocument();
  });
});
