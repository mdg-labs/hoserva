import { describe, expect, it } from "vitest";

import {
  COMMUNITY,
  entryCategories,
  entryMaintainers,
  filterEntries,
  hasFilters,
  INSTALLED,
  NO_FILTERS,
  NOT_INSTALLED,
  VERIFIED,
  type CatalogEntry,
} from "@/routes/apps/catalog-filter";

function entry(id: string, extra: Partial<CatalogEntry> = {}): CatalogEntry {
  return {
    id,
    revision: 1,
    title: id.toUpperCase(),
    categories: ["media"],
    docs: "",
    source: "hoserva",
    sourceKind: "curated",
    signed: true,
    installed: false,
    ...extra,
  };
}

const ENTRIES = [
  entry("jellyfin", { installed: true }),
  entry("notes", { categories: ["productivity", "tools"] }),
  entry("paste", { categories: ["tools"], sourceKind: "user_added", signed: false }),
];

const MAINTAINED = [
  entry("jellyfin", { maintainer: "LinuxServer.io" }),
  entry("notes", { maintainer: "Example Team" }),
  entry("agent", { maintainer: "LinuxServer.io" }),
  entry("paste"),
];

const ids = (entries: CatalogEntry[]) => entries.map((e) => e.id);

describe("filterEntries", () => {
  it("keeps everything when no filter is set", () => {
    expect(ids(filterEntries(ENTRIES, NO_FILTERS))).toEqual(["jellyfin", "notes", "paste"]);
    expect(hasFilters(NO_FILTERS)).toBe(false);
  });

  it("searches the title, the id and the categories, ignoring case and surrounding spaces", () => {
    expect(ids(filterEntries(ENTRIES, { ...NO_FILTERS, search: "  JELLY " }))).toEqual(["jellyfin"]);
    expect(ids(filterEntries(ENTRIES, { ...NO_FILTERS, search: "product" }))).toEqual(["notes"]);
  });

  it("matches an entry in any of the picked categories", () => {
    expect(ids(filterEntries(ENTRIES, { ...NO_FILTERS, categories: ["tools", "media"] }))).toEqual([
      "jellyfin",
      "notes",
      "paste",
    ]);
    expect(ids(filterEntries(ENTRIES, { ...NO_FILTERS, categories: ["productivity"] }))).toEqual(["notes"]);
  });

  it("filters by installed and by whether the source is signed", () => {
    expect(ids(filterEntries(ENTRIES, { ...NO_FILTERS, installed: INSTALLED }))).toEqual(["jellyfin"]);
    expect(ids(filterEntries(ENTRIES, { ...NO_FILTERS, installed: NOT_INSTALLED }))).toEqual(["notes", "paste"]);
    expect(ids(filterEntries(ENTRIES, { ...NO_FILTERS, verified: VERIFIED }))).toEqual(["jellyfin", "notes"]);
    expect(ids(filterEntries(ENTRIES, { ...NO_FILTERS, verified: COMMUNITY }))).toEqual(["paste"]);
  });

  it("matches an entry by any of the picked maintainers and never one with none", () => {
    expect(ids(filterEntries(MAINTAINED, { ...NO_FILTERS, maintainers: ["LinuxServer.io"] }))).toEqual([
      "jellyfin",
      "agent",
    ]);
    expect(ids(filterEntries(MAINTAINED, { ...NO_FILTERS, maintainers: ["LinuxServer.io", "Example Team"] }))).toEqual([
      "jellyfin",
      "notes",
      "agent",
    ]);
    expect(hasFilters({ ...NO_FILTERS, maintainers: ["Example Team"] })).toBe(true);
  });

  it("combines every filter", () => {
    const filters = {
      search: "e",
      categories: ["tools"],
      maintainers: [],
      installed: NOT_INSTALLED,
      verified: VERIFIED,
    };
    expect(ids(filterEntries(ENTRIES, filters))).toEqual(["notes"]);
    expect(hasFilters(filters)).toBe(true);
  });
});

describe("entryCategories", () => {
  it("lists each category once, sorted", () => {
    expect(entryCategories(ENTRIES)).toEqual(["media", "productivity", "tools"]);
  });
});

describe("entryMaintainers", () => {
  it("lists each maintainer once, sorted, leaving out entries that name none", () => {
    expect(entryMaintainers(MAINTAINED)).toEqual(["Example Team", "LinuxServer.io"]);
    expect(entryMaintainers(ENTRIES)).toEqual([]);
  });
});
