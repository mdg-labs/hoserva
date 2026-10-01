export const LOG_LINE_CHOICES = [100, 500, 2000] as const;
export const DEFAULT_LOG_LINES = 500;

const MAX_LOG_CHARS = 2_000_000;

// A followed log grows for as long as the tab stays open, so the oldest whole
// lines are dropped once the text passes a size the page can still render.
export function appendLog(text: string, chunk: string): string {
  const joined = text + chunk;
  if (joined.length <= MAX_LOG_CHARS) {
    return joined;
  }
  const cut = joined.length - MAX_LOG_CHARS;
  const newline = joined.indexOf("\n", cut);
  return newline === -1 ? joined.slice(cut) : joined.slice(newline + 1);
}
