import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { CatalogPage } from "@/routes/apps/catalog";

const mockGet = vi.fn();
const mockPost = vi.fn();
const mockToast = vi.fn();
let emitEvent: (event: unknown) => void = () => {};

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
  },
}));

vi.mock("@/components/patterns/feedback-toast", () => ({
  showFeedbackToast: (...args: unknown[]) => mockToast(...args),
}));

vi.mock("@/lib/api/events", () => ({
  subscribeToEvents: (onEvent: (event: unknown) => void) => {
    emitEvent = onEvent;
    return () => {
      emitEvent = () => {};
    };
  },
}));

function ok<T>(data: T) {
  return Promise.resolve({ data, response: { ok: true } });
}

function fail(code: string, message: string) {
  return Promise.resolve({ error: { code, message }, response: { ok: false } });
}

type Entry = {
  id: string;
  revision: number;
  title: string;
  categories: string[];
  docs: string;
  source: string;
  sourceKind: "curated" | "user_added";
  signed: boolean;
  installed: boolean;
};

function entry(id: string, title: string, extra: Partial<Entry> = {}): Entry {
  return {
    id,
    revision: 1,
    title,
    categories: ["media"],
    docs: "https://example.org/docs",
    source: "hoserva",
    sourceKind: "curated",
    signed: true,
    installed: false,
    ...extra,
  };
}

const JELLYFIN = entry("jellyfin", "Jellyfin", { installed: true });
const NOTES = entry("aio-notes", "Notes (all in one)", { categories: ["productivity"] });
const QUICKPASTE = entry("quickpaste", "Quick Paste", {
  categories: ["tools"],
  source: "src-0000000000",
  sourceKind: "user_added",
  signed: false,
});

function catalog(templates: Entry[], extra: Record<string, unknown> = {}) {
  return { serial: 3, templates, ...extra };
}

function installGet(list: () => Promise<unknown>): void {
  mockGet.mockImplementation((path: string) => {
    if (path === "/catalog") {
      return list();
    }
    return fail("not_found", `unexpected ${path}`);
  });
}

function renderPage(): ReturnType<typeof render> {
  return render(
    <MemoryRouter>
      <CatalogPage />
    </MemoryRouter>,
  );
}

function cardOf(title: string): HTMLElement {
  const link = screen.getByRole("link", { name: title });
  const card = link.closest("li");
  if (!card) {
    throw new Error(`no card for ${title}`);
  }
  return card;
}

describe("CatalogPage", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockToast.mockReset();
  });

  it("badges each entry with the source and signature the API reported", async () => {
    installGet(() => ok(catalog([JELLYFIN, QUICKPASTE])));
    renderPage();

    await screen.findByRole("link", { name: "Jellyfin" });
    expect(within(cardOf("Jellyfin")).getByText("Hoserva catalog")).toBeInTheDocument();
    expect(within(cardOf("Jellyfin")).getByText("Installed")).toBeInTheDocument();
    expect(within(cardOf("Quick Paste")).getByText("Added by you, unsigned")).toBeInTheDocument();
    expect(within(cardOf("Quick Paste")).queryByText("Installed")).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Quick Paste" })).toHaveAttribute("href", "/apps/catalog/quickpaste");
  });

  it("narrows the grid by search, category, installed and verified", async () => {
    installGet(() => ok(catalog([JELLYFIN, NOTES, QUICKPASTE])));
    renderPage();
    await screen.findByRole("link", { name: "Jellyfin" });

    fireEvent.change(screen.getByRole("textbox", { name: "Search apps" }), { target: { value: "paste" } });
    expect(screen.queryByRole("link", { name: "Jellyfin" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Quick Paste" })).toBeInTheDocument();
    fireEvent.change(screen.getByRole("textbox", { name: "Search apps" }), { target: { value: "" } });

    const verified = screen.getByRole("group", { name: "Where the app comes from" });
    fireEvent.click(within(verified).getByRole("button", { name: "Community (unsigned)" }));
    expect(screen.getAllByRole("link").filter((l) => l.getAttribute("href")?.startsWith("/apps/catalog/"))).toHaveLength(1);
    expect(screen.getByRole("link", { name: "Quick Paste" })).toBeInTheDocument();
    fireEvent.click(within(verified).getByRole("button", { name: "Community (unsigned)" }));
    expect(screen.getByRole("link", { name: "Jellyfin" })).toBeInTheDocument();

    const installed = screen.getByRole("group", { name: "Installed on this server" });
    fireEvent.click(within(installed).getByRole("button", { name: "Not installed" }));
    expect(screen.queryByRole("link", { name: "Jellyfin" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Notes (all in one)" })).toBeInTheDocument();
    fireEvent.click(within(installed).getByRole("button", { name: "Installed" }));
    expect(screen.getByRole("link", { name: "Jellyfin" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Notes (all in one)" })).not.toBeInTheDocument();
  });

  it("filters by category through the multi-pick", async () => {
    installGet(() => ok(catalog([JELLYFIN, NOTES, QUICKPASTE])));
    renderPage();
    await screen.findByRole("link", { name: "Jellyfin" });

    const categories = screen.getByRole("combobox", { name: "Filter by category" });
    fireEvent.focus(categories);
    fireEvent.keyDown(categories, { key: "ArrowDown" });
    fireEvent.click(await screen.findByRole("option", { name: "Tools" }));

    fireEvent.keyDown(categories, { key: "Escape" });

    await waitFor(() => expect(screen.getByRole("link", { name: "Quick Paste" })).toBeInTheDocument());
    expect(screen.queryByRole("link", { name: "Jellyfin" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Notes (all in one)" })).not.toBeInTheDocument();
  });

  it("explains an empty result and clears the filters from it", async () => {
    installGet(() => ok(catalog([JELLYFIN, NOTES])));
    renderPage();
    await screen.findByRole("link", { name: "Jellyfin" });

    fireEvent.change(screen.getByRole("textbox", { name: "Search apps" }), { target: { value: "zzz" } });
    expect(screen.getByText("No apps match")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Clear search and filters" }));
    expect(screen.getByRole("link", { name: "Jellyfin" })).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "Search apps" })).toHaveValue("");
  });

  it("says the catalog is empty rather than showing no matches", async () => {
    installGet(() => ok(catalog([])));
    renderPage();

    expect(await screen.findByText("The catalog is empty")).toBeInTheDocument();
    expect(screen.queryByText("No apps match")).not.toBeInTheDocument();
  });

  it("pages through the entries and starts again from page one when a filter changes", async () => {
    const many = Array.from({ length: 30 }, (_, i) =>
      entry(`app-${String(i).padStart(2, "0")}`, `App ${String(i).padStart(2, "0")}`),
    );
    installGet(() => ok(catalog(many)));
    renderPage();

    await screen.findByRole("link", { name: "App 00" });
    expect(screen.getByText("1-12 of 30")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "App 12" })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByRole("link", { name: "App 12" })).toBeInTheDocument();
    expect(screen.getByText("13-24 of 30")).toBeInTheDocument();

    fireEvent.change(screen.getByRole("textbox", { name: "Search apps" }), { target: { value: "app" } });
    expect(screen.getByText("1-12 of 30")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Previous" })).toBeDisabled();
  });

  it("shows a failed load as an error, never as an empty catalog, and retries", async () => {
    let calls = 0;
    installGet(() => {
      calls += 1;
      return calls === 1 ? fail("catalog_unavailable", "the catalog is not installed") : ok(catalog([JELLYFIN]));
    });
    renderPage();

    expect(await screen.findByText("the catalog is not installed")).toBeInTheDocument();
    expect(screen.queryByText("The catalog is empty")).not.toBeInTheDocument();
    expect(screen.queryByText("No apps match")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("link", { name: "Jellyfin" })).toBeInTheDocument();
    expect(screen.queryByText("the catalog is not installed")).not.toBeInTheDocument();
  });

  it("shows when the catalog was last checked, and when no check has run", async () => {
    installGet(() => ok(catalog([JELLYFIN], { lastCheckedAt: "2026-10-01T06:00:00Z", lastOutcome: "unchanged" })));
    renderPage();

    expect(
      await screen.findByText(`Last checked ${new Date("2026-10-01T06:00:00Z").toLocaleString()}`),
    ).toBeInTheDocument();
    expect(screen.queryByText("Last check failed")).not.toBeInTheDocument();

    cleanup();
    installGet(() => ok(catalog([JELLYFIN], { lastCheckedAt: "2026-10-01T06:00:00Z", lastOutcome: "failed" })));
    renderPage();
    expect(await screen.findByText("Last check failed")).toBeInTheDocument();

    cleanup();
    installGet(() => ok(catalog([JELLYFIN])));
    renderPage();
    expect(await screen.findByText("Not checked since the server started")).toBeInTheDocument();
  });

  it("runs one check when the button is pressed twice, shows it loading, and toasts the counts", async () => {
    installGet(() => ok(catalog([JELLYFIN])));
    let finish: (value: unknown) => void = () => {};
    mockPost.mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    );
    renderPage();
    const button = await screen.findByRole("button", { name: "Check for updates" });

    fireEvent.click(button);
    await waitFor(() => expect(button).toHaveAttribute("data-loading"));
    fireEvent.click(button);
    expect(mockPost).toHaveBeenCalledTimes(1);
    expect(mockPost).toHaveBeenCalledWith("/catalog/refresh");

    await act(async () => {
      finish({
        data: { checkedAt: "2026-10-02T08:00:00Z", outcome: "updated", newTemplates: 2, updatedTemplates: 1 },
        response: { ok: true },
      });
    });
    await waitFor(() => expect(mockToast).toHaveBeenCalledWith({ type: "success", title: "2 new, 1 updated" }));
    await waitFor(() => expect(button).not.toHaveAttribute("data-loading"));
  });

  it("toasts an unchanged catalog as up to date", async () => {
    installGet(() => ok(catalog([JELLYFIN])));
    mockPost.mockResolvedValue({
      data: { checkedAt: "2026-10-02T08:00:00Z", outcome: "unchanged" },
      response: { ok: true },
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Check for updates" }));
    await waitFor(() => expect(mockToast).toHaveBeenCalledWith({ type: "success", title: "Catalog is up to date" }));
  });

  it("toasts the reason a check failed, as an error and not as up to date", async () => {
    installGet(() => ok(catalog([JELLYFIN])));
    mockPost.mockResolvedValue({
      data: {
        checkedAt: "2026-10-02T08:00:00Z",
        outcome: "failed",
        reason: "bad_signature",
        message: "the signature does not verify",
      },
      response: { ok: true },
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Check for updates" }));
    await waitFor(() =>
      expect(mockToast).toHaveBeenCalledWith({
        type: "error",
        title: "The catalog check failed",
        description: "the signature does not verify",
      }),
    );
    expect(mockToast).not.toHaveBeenCalledWith(expect.objectContaining({ type: "success" }));
  });

  it("toasts the generic failure for a reason this build has no words for", async () => {
    installGet(() => ok(catalog([JELLYFIN])));
    mockPost.mockResolvedValue({
      data: { checkedAt: "2026-10-02T08:00:00Z", outcome: "failed", reason: "rate_limited" },
      response: { ok: true },
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Check for updates" }));
    await waitFor(() =>
      expect(mockToast).toHaveBeenCalledWith({
        type: "error",
        title: "The catalog check failed",
        description: "The catalog check did not finish.",
      }),
    );
  });

  it("toasts an API error and a rejected request as failures", async () => {
    installGet(() => ok(catalog([JELLYFIN])));
    mockPost.mockResolvedValueOnce({ error: { code: "forbidden", message: "admins only" }, response: { ok: false } });
    mockPost.mockRejectedValueOnce(new Error("network down"));
    renderPage();
    const button = await screen.findByRole("button", { name: "Check for updates" });

    fireEvent.click(button);
    await waitFor(() =>
      expect(mockToast).toHaveBeenLastCalledWith({
        type: "error",
        title: "The catalog check failed",
        description: "admins only",
      }),
    );
    await waitFor(() => expect(button).not.toHaveAttribute("data-loading"));

    fireEvent.click(button);
    await waitFor(() =>
      expect(mockToast).toHaveBeenLastCalledWith({
        type: "error",
        title: "The catalog check failed",
        description: "network down",
      }),
    );
    expect(mockToast).not.toHaveBeenCalledWith(expect.objectContaining({ type: "success" }));
  });

  it("reloads the grid when a catalog event arrives, whoever started the check", async () => {
    let calls = 0;
    installGet(() => {
      calls += 1;
      return ok(
        calls === 1
          ? catalog([JELLYFIN])
          : catalog([JELLYFIN, NOTES], { lastCheckedAt: "2026-10-02T09:00:00Z", lastOutcome: "updated" }),
      );
    });
    renderPage();
    await screen.findByRole("link", { name: "Jellyfin" });
    expect(screen.queryByRole("link", { name: "Notes (all in one)" })).not.toBeInTheDocument();

    act(() => {
      emitEvent({ event: "job_progress", data: {} });
    });
    expect(calls).toBe(1);

    act(() => {
      emitEvent({ event: "catalog", data: { checkedAt: "2026-10-02T09:00:00Z", outcome: "updated" } });
    });
    expect(await screen.findByRole("link", { name: "Notes (all in one)" })).toBeInTheDocument();
    expect(
      screen.getByText(`Last checked ${new Date("2026-10-02T09:00:00Z").toLocaleString()}`),
    ).toBeInTheDocument();
    expect(mockToast).not.toHaveBeenCalled();
  });
});
