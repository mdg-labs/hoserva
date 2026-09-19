import type { ReactNode } from "react";

import { Label } from "@/components/ui/label";
import { Radio, RadioGroup } from "@/components/ui/radio-group";
import { cn } from "@/lib/utils";

export interface SegmentedChoiceOption {
  value: string;
  label: ReactNode;
}

export function SegmentedChoice({
  value,
  onChange,
  options,
  name,
  disabled = false,
}: {
  value: string;
  onChange: (value: string) => void;
  options: SegmentedChoiceOption[];
  name: string;
  disabled?: boolean;
}): React.ReactElement {
  return (
    <RadioGroup
      value={value}
      onValueChange={onChange}
      name={name}
      disabled={disabled}
      className="inline-flex flex-wrap gap-1 rounded-lg border bg-muted/40 p-1"
    >
      {options.map((option) => (
        <Label
          key={option.value}
          className={cn(
            "cursor-pointer rounded-md px-3 py-1.5 text-sm transition-colors hover:bg-accent/50 has-data-checked:bg-background has-data-checked:shadow-xs/5",
            value === option.value && "bg-background shadow-xs/5",
            disabled && "cursor-not-allowed opacity-64",
          )}
        >
          <Radio value={option.value} className="sr-only" />
          {option.label}
        </Label>
      ))}
    </RadioGroup>
  );
}
