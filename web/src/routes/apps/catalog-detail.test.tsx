import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { CatalogDetailPage } from "@/routes/apps/catalog-detail";

const mockGet = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
  },
}));

function ok<T>(data: T) {
  return Promise.resolve({ data, response: { ok: true } });
}

function fail(code: string, message: string) {
  return Promise.resolve({ error: { code, message }, response: { ok: false } });
}

const COMPOSE = "services:\n  agent:\n    image: example/agent:1\n    privileged: true\n";

function template(extra: Record<string, unknown> = {}) {
  return {
    id: "risky-agent",
    revision: 3,
    title: "Risky agent",
    categories: ["system"],
    docs: "https://example.com/agent/docs",
    source: "hoserva",
    sourceKind: "curated",
    signed: true,
    compose: COMPOSE,
    privileges: [],
    ...extra,
  };
}

const PRIVILEGES = [
  {
    kind: "privileged",
    service: "agent",
    description: "Runs with every capability of the server.",
  },
  {
    kind: "added_capabilities",
    service: "agent",
    detail: "SYS_ADMIN",
    description: "Gets extra Linux permissions.",
  },
];

const RICH = {
  maintainer: "Example Team",
  description: "First paragraph.\n\nSecond paragraph.",
  screenshotCount: 2,
  links: {
    project: "https://example.com/project",
    support: "https://example.com/support",
    donate: "https://example.com/donate",
  },
};

function renderAt(id: string): ReturnType<typeof render> {
  return render(
    <MemoryRouter initialEntries={[`/apps/catalog/${id}`]}>
      <Routes>
        <Route path="/apps/catalog" element={<p>catalog list</p>} />
        <Route path="/apps/catalog/:appId" element={<CatalogDetailPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("CatalogDetailPage", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
  });

  it("renders the privilege summary exactly as the API computed it, with the install link", async () => {
    mockGet.mockImplementation(() => ok(template({ privileges: PRIVILEGES })));
    renderAt("risky-agent");

    expect(await screen.findByRole("heading", { name: "Risky agent" })).toBeInTheDocument();
    expect(mockGet).toHaveBeenCalledWith("/catalog/{id}", expect.objectContaining({ params: { path: { id: "risky-agent" } } }));
    expect(screen.getByText("Runs with every capability of the server.")).toBeInTheDocument();
    expect(screen.getByText("Full access to the server")).toBeInTheDocument();
    expect(screen.getByText("Extra Linux permissions")).toBeInTheDocument();
    expect(screen.getByText("SYS_ADMIN")).toBeInTheDocument();
    expect(screen.queryByText("Nothing beyond an ordinary container")).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Install" })).toHaveAttribute("href", "/apps/install/risky-agent");
    expect(screen.getByText("Hoserva catalog")).toBeInTheDocument();
  });

  it("says plainly that a template asks for nothing extra", async () => {
    mockGet.mockImplementation(() => ok(template()));
    renderAt("risky-agent");

    expect(await screen.findByText("Nothing beyond an ordinary container")).toBeInTheDocument();
  });

  it("warns that an unsigned user-added template is unsigned and still shows what it asks for", async () => {
    mockGet.mockImplementation(() =>
      ok(
        template({
          id: "quickpaste",
          title: "Quick Paste",
          source: "src-0000000000",
          sourceKind: "user_added",
          signed: false,
          privileges: [PRIVILEGES[0]],
        }),
      ),
    );
    renderAt("quickpaste");

    expect(await screen.findByText("Added by you, unsigned")).toBeInTheDocument();
    expect(screen.getByText(/It has no signature, so nothing confirms who wrote it/)).toBeInTheDocument();
    expect(screen.getByText("Runs with every capability of the server.")).toBeInTheDocument();
  });

  it("offers the raw template as read-only text", async () => {
    mockGet.mockImplementation(() => ok(template()));
    renderAt("risky-agent");

    fireEvent.click(await screen.findByRole("button", { name: /Raw template/ }));
    const code = await screen.findByText((_, node) => node?.tagName === "CODE" && node.textContent === COMPOSE);
    expect(code).toBeInTheDocument();
  });

  it("links the documentation only when it is a web address", async () => {
    mockGet.mockImplementation(() => ok(template()));
    renderAt("risky-agent");
    const docs = await screen.findByRole("link", { name: "Documentation" });
    expect(docs).toHaveAttribute("href", "https://example.com/agent/docs");
    expect(docs).toHaveAttribute("rel", "noopener noreferrer");

    cleanup();
    mockGet.mockImplementation(() => ok(template({ docs: "javascript:alert(1)" })));
    renderAt("risky-agent");
    await screen.findByRole("heading", { name: "Risky agent" });
    expect(screen.queryByRole("link", { name: "Documentation" })).not.toBeInTheDocument();
  });

  it("tells an unknown template apart from a failed load", async () => {
    mockGet.mockImplementation(() => fail("template_not_found", "no such template"));
    renderAt("nope");

    expect(await screen.findByText('"nope" is not in the catalog')).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Install" })).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("shows a failed load as an error with a retry, and never offers Install", async () => {
    let calls = 0;
    mockGet.mockImplementation(() => {
      calls += 1;
      return calls === 1 ? fail("template_invalid", "the template is invalid") : ok(template());
    });
    renderAt("risky-agent");

    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("the template is invalid")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Install" })).not.toBeInTheDocument();
    expect(screen.queryByText('"risky-agent" is not in the catalog')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("link", { name: "Install" })).toBeInTheDocument();
  });

  it("shows the maintainer, the description, the screenshots and the links when the template has them", async () => {
    mockGet.mockImplementation(() => ok(template(RICH)));
    renderAt("risky-agent");

    expect(await screen.findByText("Maintained by Example Team")).toBeInTheDocument();
    const about = screen.getByText(/First paragraph\./);
    expect(about.textContent).toBe("First paragraph.\n\nSecond paragraph.");
    expect(about.tagName).toBe("P");

    const first = screen.getByRole("img", { name: "Screenshot 1 of Risky agent" });
    expect(first).toHaveAttribute("src", "/api/v1/catalog/risky-agent/screenshots/0");
    expect(screen.getByRole("img", { name: "Screenshot 2 of Risky agent" })).toHaveAttribute(
      "src",
      "/api/v1/catalog/risky-agent/screenshots/1",
    );

    for (const [name, href] of [
      ["Project page", "https://example.com/project"],
      ["Get help", "https://example.com/support"],
      ["Support the project", "https://example.com/donate"],
    ]) {
      const link = screen.getByRole("link", { name });
      expect(link).toHaveAttribute("href", href);
      expect(link).toHaveAttribute("target", "_blank");
      expect(link).toHaveAttribute("rel", "noopener noreferrer");
    }
  });

  it("shows none of it, and no empty section, when the template has none", async () => {
    mockGet.mockImplementation(() => ok(template({ screenshotCount: 0 })));
    renderAt("risky-agent");

    await screen.findByRole("heading", { name: "Risky agent" });
    expect(screen.queryByText(/Maintained by/)).not.toBeInTheDocument();
    expect(screen.queryByText("About this app")).not.toBeInTheDocument();
    expect(screen.queryByText("Screenshots")).not.toBeInTheDocument();
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
    for (const name of ["Project page", "Get help", "Support the project"]) {
      expect(screen.queryByRole("link", { name })).not.toBeInTheDocument();
    }
  });

  it("offers only the links the template sets", async () => {
    mockGet.mockImplementation(() =>
      ok(template({ links: { donate: "https://example.com/donate" }, screenshotCount: 0 })),
    );
    renderAt("risky-agent");

    expect(await screen.findByRole("link", { name: "Support the project" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Project page" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Get help" })).not.toBeInTheDocument();
  });

  it("links only an https address with a host and no credentials, whatever the answer says", async () => {
    mockGet.mockImplementation(() =>
      ok(
        template({
          screenshotCount: 0,
          links: {
            project: "http://example.com/plain",
            support: "javascript:alert(1)",
            donate: "https://user:pw@example.com/",
          },
        }),
      ),
    );
    renderAt("risky-agent");

    await screen.findByRole("heading", { name: "Risky agent" });
    for (const name of ["Project page", "Get help", "Support the project"]) {
      expect(screen.queryByRole("link", { name })).not.toBeInTheDocument();
    }

    for (const bad of ["data:text/html,x", "https://", "https://exa mple.com/", "https://example.com/\u0007", "//example.com/", "/relative"]) {
      cleanup();
      mockGet.mockImplementation(() => ok(template({ screenshotCount: 0, links: { project: bad } })));
      renderAt("risky-agent");
      await screen.findByRole("heading", { name: "Risky agent" });
      expect(screen.queryByRole("link", { name: "Project page" }), bad).not.toBeInTheDocument();
    }
  });

  it("renders the description as text, never as markup", async () => {
    mockGet.mockImplementation(() =>
      ok(template({ screenshotCount: 0, description: "<img src=x onerror=alert(1)><b>bold</b>" })),
    );
    renderAt("risky-agent");

    expect(await screen.findByText("<img src=x onerror=alert(1)><b>bold</b>")).toBeInTheDocument();
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
  });

  it("replaces a screenshot that fails to load and keeps the page and the other screenshot", async () => {
    mockGet.mockImplementation(() => ok(template(RICH)));
    renderAt("risky-agent");

    const first = await screen.findByRole("img", { name: "Screenshot 1 of Risky agent" });
    fireEvent.error(first);

    expect(screen.getByText("This screenshot could not be loaded.")).toBeInTheDocument();
    expect(screen.queryByRole("img", { name: "Screenshot 1 of Risky agent" })).not.toBeInTheDocument();
    expect(screen.getByRole("img", { name: "Screenshot 2 of Risky agent" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Risky agent" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Install" })).toBeInTheDocument();
  });
});
