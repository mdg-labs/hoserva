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
});
