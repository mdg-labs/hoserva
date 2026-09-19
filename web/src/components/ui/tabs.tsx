"use client";

import { Tabs as TabsPrimitive } from "@base-ui/react/tabs";
import type React from "react";
import { cn } from "@/lib/utils";

export const Tabs: typeof TabsPrimitive.Root = TabsPrimitive.Root;

export function TabsList({
  className,
  ...props
}: TabsPrimitive.List.Props): React.ReactElement {
  return (
    <TabsPrimitive.List
      className={cn("flex flex-wrap gap-1 border-b", className)}
      data-slot="tabs-list"
      {...props}
    />
  );
}

export function TabsTab({
  className,
  ...props
}: TabsPrimitive.Tab.Props): React.ReactElement {
  return (
    <TabsPrimitive.Tab
      className={cn(
        "rounded-t-lg px-3 py-2 text-sm font-medium text-muted-foreground outline-none data-selected:border-b-2 data-selected:border-primary data-selected:text-foreground",
        className,
      )}
      data-slot="tabs-tab"
      {...props}
    />
  );
}

export function TabsPanel({
  className,
  ...props
}: TabsPrimitive.Panel.Props): React.ReactElement {
  return (
    <TabsPrimitive.Panel
      className={cn("pt-4 outline-none", className)}
      data-slot="tabs-panel"
      {...props}
    />
  );
}
