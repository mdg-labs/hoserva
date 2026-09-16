// Light and dark themes follow the system preference (doc 03 "Cross-cutting
// rules"): coss's generated theme (src/index.css) gates dark tokens behind
// a `.dark` ancestor class, so this is the one place that toggles it from
// `prefers-color-scheme`, with no manual override yet (none is in scope
// for this issue).
const DARK_QUERY = "(prefers-color-scheme: dark)";

function applyTheme(isDark: boolean): void {
  document.documentElement.classList.toggle("dark", isDark);
}

export function watchSystemTheme(): () => void {
  const media = window.matchMedia(DARK_QUERY);
  applyTheme(media.matches);

  const listener = (event: MediaQueryListEvent): void => applyTheme(event.matches);
  media.addEventListener("change", listener);
  return () => media.removeEventListener("change", listener);
}
