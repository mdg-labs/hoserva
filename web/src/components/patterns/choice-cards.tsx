import type { ReactNode } from "react";

import { Label } from "@/components/ui/label";
import { Radio, RadioGroup } from "@/components/ui/radio-group";
import { cn } from "@/lib/utils";

export interface ChoiceCardOption {
  value: string;
  title: string;
  description: string;
  icon?: ReactNode;
}

export function ChoiceCards({
  value,
  onChange,
  options,
  name,
}: {
  value: string;
  onChange: (value: string) => void;
  options: ChoiceCardOption[];
  name: string;
}): React.ReactElement {
  return (
    <RadioGroup value={value} onValueChange={onChange} name={name} className="grid gap-3 sm:grid-cols-2">
      {options.map((option) => (
        <Label
          key={option.value}
          className={cn(
            "flex cursor-pointer flex-col gap-2 rounded-xl border bg-card p-4 shadow-xs/5 transition-colors hover:bg-accent/30 has-data-checked:border-primary has-data-checked:ring-1 has-data-checked:ring-primary/30",
            value === option.value && "border-primary ring-1 ring-primary/30",
          )}
        >
          <div className="flex items-start gap-3">
            {option.icon ? <span className="text-muted-foreground">{option.icon}</span> : null}
            <div className="flex flex-1 flex-col gap-1">
              <div className="flex items-center gap-2">
                <Radio value={option.value} />
                <span className="font-medium">{option.title}</span>
              </div>
              <p className="text-muted-foreground text-sm">{option.description}</p>
            </div>
          </div>
        </Label>
      ))}
    </RadioGroup>
  );
}
