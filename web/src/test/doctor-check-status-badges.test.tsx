// #417: every doctor-check status badge must render a catalog string for
// pass/warn/fail (and a catalog fallback for anything else) instead of the
// raw API enum value.

import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { StackedChecks } from "@/components/patterns/stacked-checks";
import type { components } from "@/lib/api/client";
import { WelcomePage } from "@/routes/welcome";

type DoctorCheck = components["schemas"]["DoctorCheck"];

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
    phase: "welcome",
    user: null,
    adminExists: false,
    refresh: vi.fn(),
    acceptSession: vi.fn(),
  }),
}));

afterEach(() => {
  cleanup();
});

describe("doctor check status badges (#417)", () => {
  it("renders catalog labels for pass, warn and fail instead of the raw status", () => {
    const checks: DoctorCheck[] = [
      { id: "a", name: "Check A", status: "pass", message: "All good" },
      { id: "b", name: "Check B", status: "warn", message: "Needs attention" },
      { id: "c", name: "Check C", status: "fail", message: "Broken" },
    ];

    render(<StackedChecks checks={checks} />);

    expect(screen.getByText("Pass")).toBeInTheDocument();
    expect(screen.getByText("Warning")).toBeInTheDocument();
    expect(screen.getByText("Failed")).toBeInTheDocument();
    expect(screen.queryByText("pass")).not.toBeInTheDocument();
    expect(screen.queryByText("warn")).not.toBeInTheDocument();
    expect(screen.queryByText("fail")).not.toBeInTheDocument();
  });

  it("renders the catalog fallback for an unrecognised status instead of the raw value", () => {
    const checks = [
      { id: "d", name: "Check D", status: "unexpected", message: "Unclear" },
    ] as unknown as DoctorCheck[];

    render(<StackedChecks checks={checks} />);

    expect(screen.getByText("Unknown")).toBeInTheDocument();
    expect(screen.queryByText("unexpected")).not.toBeInTheDocument();
  });
});

describe("doctor check status badge on the welcome onboarding info panel (#417)", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    sessionStorage.clear();
    localStorage.clear();
  });

  it("renders the catalog label, not the raw status, for a failed docker-volumes listing", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/doctor") {
        return Promise.resolve({
          data: {
            checks: [
              { id: "host_samba", name: "Samba shares", status: "pass", message: "1 share: media" },
              {
                id: "host_docker_volumes",
                name: "Docker volumes",
                status: "warn",
                message: "Could not list Docker volumes: docker volume ls: timed out",
              },
            ],
          },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    sessionStorage.setItem("hoserva.onboardingStep", "1");
    render(
      <MemoryRouter>
        <WelcomePage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("Could not list Docker volumes: docker volume ls: timed out")).toBeInTheDocument();
    expect(screen.getByText("Warning")).toBeInTheDocument();
    expect(screen.queryByText("warn")).not.toBeInTheDocument();
  });
});
