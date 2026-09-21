import type * as React from "react";
import { cn } from "@/lib/utils";

export function Fieldset({
  className,
  ...props
}: React.ComponentProps<"fieldset">): React.ReactElement {
  return (
    <fieldset
      className={cn(
        "flex flex-col gap-4 rounded-2xl border bg-card p-6 not-disabled:shadow-xs/5 disabled:opacity-64",
        className,
      )}
      data-slot="fieldset"
      {...props}
    />
  );
}

export function FieldsetLegend({
  className,
  ...props
}: React.ComponentProps<"legend">): React.ReactElement {
  return (
    <legend
      className={cn("-mt-2 px-1 font-semibold font-heading text-base", className)}
      data-slot="fieldset-legend"
      {...props}
    />
  );
}
