import type { ReactNode } from "react";

export function PlainTerm({
  label,
  term,
}: {
  label: ReactNode;
  term: string;
}): React.ReactElement {
  return (
    <span className="inline-flex flex-wrap items-baseline gap-x-1.5 gap-y-0.5">
      <span>{label}</span>
      <span className="font-mono text-muted-foreground text-xs">({term})</span>
    </span>
  );
}
