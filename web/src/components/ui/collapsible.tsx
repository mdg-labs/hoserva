"use client";

import { Collapsible as CollapsiblePrimitive } from "@base-ui/react/collapsible";
import type * as React from "react";
import { cn } from "@/lib/utils";

export const Collapsible: typeof CollapsiblePrimitive.Root = CollapsiblePrimitive.Root;

export function CollapsibleTrigger({
  className,
  ...props
}: CollapsiblePrimitive.Trigger.Props): React.ReactElement {
  return (
    <CollapsiblePrimitive.Trigger
      className={cn(
        "flex w-full items-center justify-between gap-2 rounded-lg px-1 py-2 text-left text-sm font-medium hover:bg-muted/50",
        className,
      )}
      data-slot="collapsible-trigger"
      {...props}
    />
  );
}

export function CollapsiblePanel({
  className,
  ...props
}: CollapsiblePrimitive.Panel.Props): React.ReactElement {
  return (
    <CollapsiblePrimitive.Panel
      className={cn("overflow-hidden data-[ending-style]:animate-out data-[starting-style]:animate-in", className)}
      data-slot="collapsible-panel"
      {...props}
    />
  );
}
