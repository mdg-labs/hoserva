import { useEffect, useRef } from "react";
import { useTranslation } from "react-i18next";

import { ScrollArea } from "@/components/ui/scroll-area";

type Segment = { text: string; match: boolean };

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function segmentsOf(text: string, query: string): Segment[] {
  if (query === "") {
    return [{ text, match: false }];
  }
  const segments: Segment[] = [];
  let last = 0;
  for (const found of text.matchAll(new RegExp(escapeRegExp(query), "gi"))) {
    if (found.index > last) {
      segments.push({ text: text.slice(last, found.index), match: false });
    }
    segments.push({ text: found[0], match: true });
    last = found.index + found[0].length;
  }
  if (last < text.length) {
    segments.push({ text: text.slice(last), match: false });
  }
  return segments;
}

export function LogView({
  content,
  loading = false,
  highlight = "",
  followEnd = false,
}: {
  content: string | null;
  loading?: boolean;
  // Case-insensitive text to mark wherever it occurs in the content.
  highlight?: string;
  // Keeps the newest line in view as the content grows.
  followEnd?: boolean;
}): React.ReactElement {
  const { t } = useTranslation();
  const endRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!followEnd) {
      return;
    }
    const viewport = endRef.current?.closest("[data-slot=scroll-area-viewport]");
    if (viewport) {
      viewport.scrollTop = viewport.scrollHeight;
    }
  }, [content, followEnd]);

  if (loading) {
    return (
      <div className="rounded-lg border bg-muted/30 p-4 text-sm text-muted-foreground" role="status">
        {t("loading.label")}
      </div>
    );
  }

  const text = content ?? t("logView.empty");
  return (
    <ScrollArea className="h-96 rounded-lg border bg-muted/20">
      <pre className="p-4 font-mono text-xs whitespace-pre-wrap break-all">
        {segmentsOf(text, content === null ? "" : highlight).map((segment, index) =>
          segment.match ? (
            <mark key={index} className="rounded-xs bg-warning/32 text-foreground">
              {segment.text}
            </mark>
          ) : (
            segment.text
          ),
        )}
      </pre>
      <div ref={endRef} />
    </ScrollArea>
  );
}
