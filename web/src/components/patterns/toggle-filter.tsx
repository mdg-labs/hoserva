// A clearable filter as a ToggleGroup (doc 03 §5.2, `p-toggle-group-4`):
// pressing the pressed option clears the filter, so "no filter" needs no
// option of its own.
import { Toggle } from "@base-ui/react/toggle";
import { ToggleGroup } from "@base-ui/react/toggle-group";

import { cn } from "@/lib/utils";

export interface ToggleFilterOption {
  value: string;
  label: string;
}

export function ToggleFilter({
  label,
  value,
  onChange,
  options,
}: {
  label: string;
  value: string | null;
  onChange: (value: string | null) => void;
  options: ToggleFilterOption[];
}): React.ReactElement {
  return (
    <ToggleGroup
      aria-label={label}
      value={value === null ? [] : [value]}
      onValueChange={(next) => onChange(next[0] ?? null)}
      className="inline-flex rounded-lg border bg-muted/40 p-1"
    >
      {options.map((option) => (
        <Toggle
          key={option.value}
          value={option.value}
          className={cn(
            "rounded-md px-3 py-1.5 text-sm outline-none transition-colors hover:bg-accent/50 focus-visible:ring-2 focus-visible:ring-ring data-pressed:bg-background data-pressed:shadow-xs/5",
          )}
        >
          {option.label}
        </Toggle>
      ))}
    </ToggleGroup>
  );
}
