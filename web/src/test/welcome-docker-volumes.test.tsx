// Issue #415: the onboarding system check must surface `host_docker_volumes`
// as an informational card inside the Q76 section, and never send it back
// as a submitted host-config choice (there is no host file for it to
// import or leave unmanaged — #413).

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { WelcomePage } from "@/routes/welcome";

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

const doctorChecksWithVolumes = [
  { id: "pkg_mergerfs", name: "mergerfs version", status: "pass", message: "mergerfs 2.40.2 is installed" },
  { id: "host_samba", name: "Samba shares", status: "pass", message: "1 share: media" },
  {
    id: "host_docker_containers",
    name: "Docker containers",
    status: "pass",
    message: "1 container: jellyfin",
  },
  { id: "host_docker_images", name: "Docker images", status: "pass", message: "1 image: jellyfin:latest" },
  { id: "host_docker_volumes", name: "Docker volumes", status: "pass", message: "1 volume(s): jellyfin-config" },
];

function renderOnStep1(): void {
  sessionStorage.setItem("hoserva.onboardingStep", "1");
  render(
    <MemoryRouter>
      <WelcomePage />
    </MemoryRouter>,
  );
}

describe("welcome onboarding — host_docker_volumes (#415)", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    sessionStorage.clear();
    localStorage.clear();
  });

  it("renders the docker volumes check as an informational card with no import/leave control", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/doctor") {
        return Promise.resolve({ data: { checks: doctorChecksWithVolumes }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderOnStep1();

    expect(await screen.findByText("1 volume(s): jellyfin-config")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Docker's storage stays at /var/lib/docker because named volumes are in use. There is nothing to import or leave unmanaged here.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryAllByRole("button", { name: "Import into Hoserva" })).toHaveLength(3);
    expect(screen.queryAllByRole("button", { name: "Leave unmanaged" })).toHaveLength(3);
  });

  it("never submits host_docker_volumes as a host-config choice", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/doctor") {
        return Promise.resolve({ data: { checks: doctorChecksWithVolumes }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPost.mockImplementation((path: string) => {
      if (path === "/doctor/host-config") {
        return Promise.resolve({ data: { files: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderOnStep1();

    await screen.findByText("1 volume(s): jellyfin-config");
    fireEvent.click(screen.getByRole("button", { name: "Next" }));

    await vi.waitFor(() => expect(mockPost).toHaveBeenCalledWith("/doctor/host-config", expect.anything()));
    const [, options] = mockPost.mock.calls.find(([path]) => path === "/doctor/host-config")!;
    const body = (options as { body: { files: Array<{ id: string }> } }).body;
    expect(body.files.map((file) => file.id)).not.toContain("host_docker_volumes");
    expect(body.files.map((file) => file.id)).toEqual(
      expect.arrayContaining(["host_samba", "host_docker_containers", "host_docker_images"]),
    );
  });

  it("renders no docker-volumes card when the doctor response omits the check", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/doctor") {
        return Promise.resolve({
          data: { checks: [{ id: "host_samba", name: "Samba shares", status: "pass", message: "1 share: media" }] },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderOnStep1();

    await screen.findByText("1 share: media");
    expect(screen.queryByText("Docker volumes")).not.toBeInTheDocument();
  });

  it("does not render a docker-volumes card when the doctor request fails", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/doctor") {
        return Promise.resolve({ error: { message: "doctor check unavailable" }, response: { ok: false } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderOnStep1();

    expect(await screen.findByText("doctor check unavailable")).toBeInTheDocument();
    expect(screen.queryByText("Docker volumes")).not.toBeInTheDocument();
  });
});
