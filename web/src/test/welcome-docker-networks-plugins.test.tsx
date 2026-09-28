// Issue #416: the onboarding system check must surface
// `host_docker_networks` and `host_docker_plugins` as informational cards
// inside the Q76 section, the same way `host_docker_volumes` already does
// (#415), and never send either back as a submitted host-config choice —
// neither has a host file to import or leave unmanaged.

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

const doctorChecksWithNetworksAndPlugins = [
  { id: "pkg_mergerfs", name: "mergerfs version", status: "pass", message: "mergerfs 2.40.2 is installed" },
  { id: "host_samba", name: "Samba shares", status: "pass", message: "1 share: media" },
  {
    id: "host_docker_containers",
    name: "Docker containers",
    status: "pass",
    message: "1 container: jellyfin",
  },
  { id: "host_docker_images", name: "Docker images", status: "pass", message: "1 image: jellyfin:latest" },
  { id: "host_docker_networks", name: "Docker networks", status: "pass", message: "1 network(s): media-net" },
  { id: "host_docker_plugins", name: "Docker plugins", status: "pass", message: "1 plugin(s): vieux/sshfs:latest" },
];

function renderOnStep1(): void {
  sessionStorage.setItem("hoserva.onboardingStep", "1");
  render(
    <MemoryRouter>
      <WelcomePage />
    </MemoryRouter>,
  );
}

describe("welcome onboarding — host_docker_networks / host_docker_plugins (#416)", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    sessionStorage.clear();
    localStorage.clear();
  });

  it("renders the networks and plugins checks as informational cards with no import/leave control", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/doctor") {
        return Promise.resolve({ data: { checks: doctorChecksWithNetworksAndPlugins }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderOnStep1();

    expect(await screen.findByText("1 network(s): media-net")).toBeInTheDocument();
    expect(screen.getByText("1 plugin(s): vieux/sshfs:latest")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Docker's storage stays at /var/lib/docker because custom networks are in use. There is nothing to import or leave unmanaged here.",
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "Docker's storage stays at /var/lib/docker because plugins are installed. There is nothing to import or leave unmanaged here.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryAllByRole("button", { name: "Import into Hoserva" })).toHaveLength(3);
    expect(screen.queryAllByRole("button", { name: "Leave unmanaged" })).toHaveLength(3);
  });

  it("never submits host_docker_networks or host_docker_plugins as a host-config choice", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/doctor") {
        return Promise.resolve({ data: { checks: doctorChecksWithNetworksAndPlugins }, response: { ok: true } });
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

    await screen.findByText("1 network(s): media-net");
    fireEvent.click(screen.getByRole("button", { name: "Next" }));

    await vi.waitFor(() => expect(mockPost).toHaveBeenCalledWith("/doctor/host-config", expect.anything()));
    const [, options] = mockPost.mock.calls.find(([path]) => path === "/doctor/host-config")!;
    const body = (options as { body: { files: Array<{ id: string }> } }).body;
    expect(body.files.map((file) => file.id)).not.toContain("host_docker_networks");
    expect(body.files.map((file) => file.id)).not.toContain("host_docker_plugins");
    expect(body.files.map((file) => file.id)).toEqual(
      expect.arrayContaining(["host_samba", "host_docker_containers", "host_docker_images"]),
    );
  });

  // The backend omits host_docker_networks/plugins entirely once Docker
  // holds neither (#416) — this also covers the empty-category case, not
  // just a doctor response that predates the checks.
  it("renders no networks/plugins cards when the doctor response omits the checks", async () => {
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
    expect(screen.queryByText("Docker networks")).not.toBeInTheDocument();
    expect(screen.queryByText("Docker plugins")).not.toBeInTheDocument();
  });

  // #416: a failed listing must never be shown with the same fixed
  // "because custom networks are in use" / "because plugins are installed"
  // text a successful, non-empty listing gets — that would claim a reason
  // the check could not actually confirm.
  it("shows an honest failure message instead of the networks/plugins-in-use reason when the listing failed", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/doctor") {
        return Promise.resolve({
          data: {
            checks: [
              { id: "host_samba", name: "Samba shares", status: "pass", message: "1 share: media" },
              {
                id: "host_docker_networks",
                name: "Docker networks",
                status: "warn",
                message: "Could not list Docker networks: docker network ls: timed out",
              },
              {
                id: "host_docker_plugins",
                name: "Docker plugins",
                status: "warn",
                message: "Could not list Docker plugins: docker plugin ls: timed out",
              },
            ],
          },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderOnStep1();

    expect(
      await screen.findByText("Could not list Docker networks: docker network ls: timed out"),
    ).toBeInTheDocument();
    expect(screen.getByText("Could not list Docker plugins: docker plugin ls: timed out")).toBeInTheDocument();
    expect(screen.getAllByText("Docker's storage stays at /var/lib/docker until this can be checked.")).toHaveLength(
      2,
    );
    expect(
      screen.queryByText(
        "Docker's storage stays at /var/lib/docker because custom networks are in use. There is nothing to import or leave unmanaged here.",
      ),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByText(
        "Docker's storage stays at /var/lib/docker because plugins are installed. There is nothing to import or leave unmanaged here.",
      ),
    ).not.toBeInTheDocument();
  });
});
