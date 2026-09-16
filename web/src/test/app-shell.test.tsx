import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { App } from "@/App";

function mockMatchMedia(prefersDark: boolean): void {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    addEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
    matches: prefersDark,
    media: query,
    onchange: null,
    removeEventListener: vi.fn(),
  })) as unknown as typeof window.matchMedia;
}

describe("App shell", () => {
  it("renders the sidebar navigation from the i18n catalog", () => {
    mockMatchMedia(false);
    render(<App />);

    expect(screen.getByText("Hoserva")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Dashboard" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Storage" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Jobs" })).toBeInTheDocument();
  });

  it("applies the dark class when the system prefers dark", () => {
    mockMatchMedia(true);
    render(<App />);

    expect(document.documentElement.classList.contains("dark")).toBe(true);
  });

  it("does not apply the dark class when the system prefers light", () => {
    mockMatchMedia(false);
    render(<App />);

    expect(document.documentElement.classList.contains("dark")).toBe(false);
  });
});
