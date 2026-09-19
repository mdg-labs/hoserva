import type { ReactNode } from "react";

import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Switch } from "@/components/ui/switch";
import { cn } from "@/lib/utils";

export function SettingSwitch({
  label,
  description,
  checked,
  onCheckedChange,
  disabled = false,
  className,
}: {
  label: ReactNode;
  description?: ReactNode;
  checked: boolean;
  onCheckedChange?: (checked: boolean) => void;
  disabled?: boolean;
  className?: string;
}): React.ReactElement {
  return (
    <Field className={cn("flex-row items-start justify-between gap-4", className)}>
      <div className="flex min-w-0 flex-col gap-1">
        <FieldLabel className="cursor-default">{label}</FieldLabel>
        {description ? <FieldDescription>{description}</FieldDescription> : null}
      </div>
      <Switch
        checked={checked}
        onCheckedChange={onCheckedChange}
        disabled={disabled}
        aria-label={typeof label === "string" ? label : undefined}
      />
    </Field>
  );
}
