export type AppEntry = {
  id: string;
  revision: number;
  title: string;
  description?: string;
  categories: string[];
  docs: string;
  maintainer?: string;
  icon?: string;
  screenshots: string[];
};

export type Catalog = {
  serial: number;
  generatedAt?: string;
  templates: AppEntry[];
};

const acronyms: Record<string, string> = {dns: 'DNS'};

export function categoryLabel(category: string): string {
  if (category in acronyms) return acronyms[category];
  const text = category.replace(/-/g, ' ');
  return text.charAt(0).toUpperCase() + text.slice(1);
}

export function builtOn(catalog: {generatedAt?: string}): string | undefined {
  return catalog.generatedAt?.slice(0, 10);
}

// A template matches when every word of the query appears in its title,
// description, maintainer or category names.
export function matches(app: AppEntry, query: string): boolean {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (words.length === 0) return true;
  const text = [
    app.title,
    app.description ?? '',
    app.maintainer ?? '',
    ...app.categories.map(categoryLabel),
  ]
    .join(' ')
    .toLowerCase();
  return words.every((word) => text.includes(word));
}
