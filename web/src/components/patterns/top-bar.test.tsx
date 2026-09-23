import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { AppShell } from "@/components/patterns/app-shell";

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

function mockMatchMedia(): void {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    addEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
    matches: false,
    media: query,
    onchange: null,
    removeEventListener: vi.fn(),
  })) as unknown as typeof window.matchMedia;
}

function mockHealthySystemData(): void {
  mockGet.mockImplementation((path: string) => {
    if (path === "/status") {
      return Promise.resolve({
        data: { healthy: true, summary: "OK", activeJobs: 0 },
        response: { ok: true },
      });
    }
    if (path === "/pool") {
      return Promise.resolve({
        data: { mounted: true, disks: [] },
        response: { ok: true },
      });
    }
    if (path === "/jobs") {
      return Promise.resolve({ data: { jobs: [] }, response: { ok: true } });
    }
    if (path === "/doctor") {
      return Promise.resolve({ data: { overall: "pass", checks: [] }, response: { ok: true } });
    }
    if (path === "/notifications") {
      return Promise.resolve({ error: { message: "notifications unavailable" }, response: { ok: false } });
    }
    return Promise.resolve({ data: null, response: { ok: false } });
  });
}

describe("TopBar notifications load failures", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockMatchMedia();
    mockHealthySystemData();
  });

  it("shows a notifications error instead of the empty inbox copy when /notifications fails", async () => {
    render(
      <MemoryRouter>
        <AppShell>
          <div>Page body</div>
        </AppShell>
      </MemoryRouter>,
    );

    await waitFor(() => {
      expect(mockGet).toHaveBeenCalledWith("/notifications", expect.anything());
    });

    fireEvent.click(screen.getByLabelText("Notifications"));

    const panel = await screen.findByRole("heading", { name: "Notifications" });
    const inbox = panel.parentElement?.parentElement;
    expect(inbox).not.toBeNull();
    expect(within(inbox as HTMLElement).getByRole("alert")).toHaveTextContent("notifications unavailable");
    expect(within(inbox as HTMLElement).queryByText("No alerts right now.")).not.toBeInTheDocument();
  });
});
