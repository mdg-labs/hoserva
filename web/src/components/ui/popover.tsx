"use client";

import { Popover as PopoverPrimitive } from "@base-ui/react/popover";
import type React from "react";
import { cn } from "@/lib/utils";

export const Popover: typeof PopoverPrimitive.Root = PopoverPrimitive.Root;

export function PopoverTrigger(
  props: PopoverPrimitive.Trigger.Props,
): React.ReactElement {
  return <PopoverPrimitive.Trigger data-slot="popover-trigger" {...props} />;
}

export function PopoverPopup({
  className,
  align = "center",
  side = "bottom",
  sideOffset = 4,
  portalProps,
  ...props
}: PopoverPrimitive.Popup.Props & {
  align?: PopoverPrimitive.Positioner.Props["align"];
  side?: PopoverPrimitive.Positioner.Props["side"];
  sideOffset?: PopoverPrimitive.Positioner.Props["sideOffset"];
  portalProps?: PopoverPrimitive.Portal.Props;
}): React.ReactElement {
  return (
    <PopoverPrimitive.Portal {...portalProps}>
      <PopoverPrimitive.Positioner
        align={align}
        className="z-50"
        data-slot="popover-positioner"
        side={side}
        sideOffset={sideOffset}
      >
        <PopoverPrimitive.Popup
          className={cn(
            "w-72 rounded-xl border bg-popover p-3 text-popover-foreground shadow-md/5 outline-none",
            className,
          )}
          data-slot="popover-popup"
          {...props}
        />
      </PopoverPrimitive.Positioner>
    </PopoverPrimitive.Portal>
  );
}

export { PopoverPopup as PopoverContent };
