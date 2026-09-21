"use client";

import { Combobox as ComboboxPrimitive } from "@base-ui/react/combobox";
import { ChevronsUpDownIcon } from "lucide-react";
import type React from "react";
import { cn } from "@/lib/utils";

export const Combobox: typeof ComboboxPrimitive.Root = ComboboxPrimitive.Root;

export function ComboboxChips({
  className,
  ...props
}: ComboboxPrimitive.Chips.Props): React.ReactElement {
  return (
    <ComboboxPrimitive.Chips
      className={cn(
        "relative flex min-h-9 w-full flex-wrap items-center gap-1.5 rounded-lg border border-input bg-background not-dark:bg-clip-padding px-[calc(--spacing(2)-1px)] py-1 text-base text-foreground shadow-xs/5 outline-none ring-ring/24 transition-shadow before:pointer-events-none before:absolute before:inset-0 before:rounded-[calc(var(--radius-lg)-1px)] not-focus-within:before:shadow-[0_1px_--theme(--color-black/4%)] focus-within:border-ring focus-within:ring-[3px] sm:min-h-8 sm:text-sm dark:bg-input/32",
        className,
      )}
      data-slot="combobox-chips"
      {...props}
    />
  );
}

export function ComboboxChip({
  className,
  ...props
}: ComboboxPrimitive.Chip.Props): React.ReactElement {
  return (
    <ComboboxPrimitive.Chip
      className={cn(
        "inline-flex items-center gap-1 rounded-sm bg-secondary px-[calc(--spacing(1)-1px)] py-0.5 text-secondary-foreground text-sm data-highlighted:bg-accent sm:text-xs",
        className,
      )}
      data-slot="combobox-chip"
      {...props}
    />
  );
}

export function ComboboxChipRemove({
  className,
  ...props
}: ComboboxPrimitive.ChipRemove.Props): React.ReactElement {
  return (
    <ComboboxPrimitive.ChipRemove
      className={cn(
        "rounded-sm opacity-70 outline-none hover:opacity-100 focus-visible:ring-2 focus-visible:ring-ring",
        className,
      )}
      data-slot="combobox-chip-remove"
      {...props}
    />
  );
}

export function ComboboxInput({
  className,
  ...props
}: ComboboxPrimitive.Input.Props): React.ReactElement {
  return (
    <ComboboxPrimitive.Input
      className={cn(
        "min-w-24 flex-1 bg-transparent px-1 py-1 text-base text-foreground outline-none placeholder:text-muted-foreground sm:text-sm",
        className,
      )}
      data-slot="combobox-input"
      {...props}
    />
  );
}

export function ComboboxTrigger({
  className,
  ...props
}: ComboboxPrimitive.Trigger.Props): React.ReactElement {
  return (
    <ComboboxPrimitive.Trigger
      className={cn(
        "-me-1 inline-flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground outline-none hover:bg-accent",
        className,
      )}
      data-slot="combobox-trigger"
      {...props}
    >
      <ChevronsUpDownIcon className="size-4" />
    </ComboboxPrimitive.Trigger>
  );
}

export function ComboboxPopup({
  className,
  children,
  sideOffset = 4,
  ...props
}: ComboboxPrimitive.Popup.Props &
  Pick<ComboboxPrimitive.Positioner.Props, "sideOffset">): React.ReactElement {
  return (
    <ComboboxPrimitive.Portal>
      <ComboboxPrimitive.Positioner className="z-50 select-none" sideOffset={sideOffset}>
        <ComboboxPrimitive.Popup
          className={cn(
            "max-h-(--available-height) min-w-(--anchor-width) origin-(--transform-origin) overflow-y-auto rounded-lg border bg-popover not-dark:bg-clip-padding p-1 text-foreground shadow-lg/5 outline-none",
            className,
          )}
          data-slot="combobox-popup"
          {...props}
        >
          {children}
        </ComboboxPrimitive.Popup>
      </ComboboxPrimitive.Positioner>
    </ComboboxPrimitive.Portal>
  );
}

export function ComboboxList({
  className,
  ...props
}: ComboboxPrimitive.List.Props): React.ReactElement {
  return (
    <ComboboxPrimitive.List
      className={cn("flex flex-col gap-0.5", className)}
      data-slot="combobox-list"
      {...props}
    />
  );
}

export function ComboboxItem({
  className,
  children,
  ...props
}: ComboboxPrimitive.Item.Props): React.ReactElement {
  return (
    <ComboboxPrimitive.Item
      className={cn(
        "grid min-h-8 cursor-default grid-cols-[1rem_minmax(0,1fr)] items-center gap-2 rounded-sm py-1 ps-2 pe-4 text-base outline-none data-disabled:pointer-events-none data-disabled:opacity-64 data-highlighted:bg-accent data-highlighted:text-accent-foreground sm:min-h-7 sm:text-sm",
        className,
      )}
      data-slot="combobox-item"
      {...props}
    >
      <ComboboxPrimitive.ItemIndicator className="col-start-1">
        <svg
          aria-hidden="true"
          fill="none"
          height="24"
          stroke="currentColor"
          strokeLinecap="round"
          strokeLinejoin="round"
          strokeWidth="2"
          viewBox="0 0 24 24"
          width="24"
          xmlns="http://www.w3.org/2000/svg"
        >
          <path d="M5.252 12.7 10.2 18.63 18.748 5.37" />
        </svg>
      </ComboboxPrimitive.ItemIndicator>
      <span className="col-start-2 min-w-0 truncate">{children}</span>
    </ComboboxPrimitive.Item>
  );
}

export function ComboboxEmpty({
  className,
  ...props
}: ComboboxPrimitive.Empty.Props): React.ReactElement {
  return (
    <ComboboxPrimitive.Empty
      className={cn("px-2 py-4 text-center text-muted-foreground text-sm", className)}
      data-slot="combobox-empty"
      {...props}
    />
  );
}

export { ComboboxPrimitive };
