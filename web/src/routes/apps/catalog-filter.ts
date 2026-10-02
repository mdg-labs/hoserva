import type { components } from "@/lib/api/client";

export type CatalogEntry = components["schemas"]["CatalogEntry"];

export const INSTALLED = "installed";
export const NOT_INSTALLED = "not-installed";
export const VERIFIED = "verified";
export const COMMUNITY = "community";

export type CatalogFilters = {
  search: string;
  categories: string[];
  installed: string | null;
  verified: string | null;
};

export const NO_FILTERS: CatalogFilters = { search: "", categories: [], installed: null, verified: null };

export function hasFilters(filters: CatalogFilters): boolean {
  return (
    filters.search.trim() !== "" ||
    filters.categories.length > 0 ||
    filters.installed !== null ||
    filters.verified !== null
  );
}

export function entryCategories(entries: CatalogEntry[]): string[] {
  return [...new Set(entries.flatMap((entry) => entry.categories))].sort((a, b) => a.localeCompare(b));
}

// Search, filters and paging are the caller's (the API lists every entry),
// so this only narrows the list by what each entry says about itself.
export function filterEntries(entries: CatalogEntry[], filters: CatalogFilters): CatalogEntry[] {
  const needle = filters.search.trim().toLowerCase();
  return entries.filter((entry) => {
    if (
      needle !== "" &&
      !entry.title.toLowerCase().includes(needle) &&
      !entry.id.toLowerCase().includes(needle) &&
      !entry.categories.some((category) => category.toLowerCase().includes(needle))
    ) {
      return false;
    }
    if (filters.categories.length > 0 && !entry.categories.some((c) => filters.categories.includes(c))) {
      return false;
    }
    if (filters.installed === INSTALLED && !entry.installed) {
      return false;
    }
    if (filters.installed === NOT_INSTALLED && entry.installed) {
      return false;
    }
    if (filters.verified === VERIFIED && !entry.signed) {
      return false;
    }
    if (filters.verified === COMMUNITY && entry.signed) {
      return false;
    }
    return true;
  });
}

export function catalogDetailPath(id: string): string {
  return `/apps/catalog/${encodeURIComponent(id)}`;
}

export function installPath(id: string): string {
  return `/apps/install/${encodeURIComponent(id)}`;
}
