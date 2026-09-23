import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { DiskDetailPage } from "@/routes/storage/disk-detail";

const mockGet = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
  },
}));

describe("DiskDetailPage load failures", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    window.matchMedia = vi.fn().mockImplementation((query: string) => ({
      addEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
      matches: false,
      media: query,
      onchange: null,
      removeEventListener: vi.fn(),
    })) as unknown as typeof window.matchMedia;
  });

  it("shows a load error instead of the not-found state when /disks fails", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/disks") {
        return Promise.resolve({ error: { message: "disks unavailable" }, response: { ok: false } });
      }
      if (path === "/pool") {
        return Promise.resolve({ data: { mounted: true, disks: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter initialEntries={["/storage/disks/%2Fdev%2Fsdb"]}>
        <Routes>
          <Route path="/storage/disks/:diskId" element={<DiskDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    expect(await screen.findByText("disks unavailable")).toBeInTheDocument();
    expect(screen.queryByText(/not found/i)).not.toBeInTheDocument();
  });
});
