"use client";

import { Menu as MenuPrimitive } from "@base-ui/react/menu";
import type React from "react";
import { cn } from "@/lib/utils";

export const Menu: typeof MenuPrimitive.Root = MenuPrimitive.Root;

export function MenuTrigger(
  props: MenuPrimitive.Trigger.Props,
): React.ReactElement {
  return <MenuPrimitive.Trigger data-slot="menu-trigger" {...props} />;
}

export function MenuPopup({
  className,
  align = "end",
  side = "bottom",
  sideOffset = 4,
  portalProps,
  ...props
}: MenuPrimitive.Popup.Props & {
  align?: MenuPrimitive.Positioner.Props["align"];
  side?: MenuPrimitive.Positioner.Props["side"];
  sideOffset?: MenuPrimitive.Positioner.Props["sideOffset"];
  portalProps?: MenuPrimitive.Portal.Props;
}): React.ReactElement {
  return (
    <MenuPrimitive.Portal {...portalProps}>
      <MenuPrimitive.Positioner
        align={align}
        className="z-50"
        data-slot="menu-positioner"
        side={side}
        sideOffset={sideOffset}
      >
        <MenuPrimitive.Popup
          className={cn(
            "min-w-40 rounded-xl border bg-popover p-1 text-popover-foreground shadow-md/5 outline-none",
            className,
          )}
          data-slot="menu-popup"
          {...props}
        />
      </MenuPrimitive.Positioner>
    </MenuPrimitive.Portal>
  );
}

export function MenuItem({
  className,
  ...props
}: MenuPrimitive.Item.Props): React.ReactElement {
  return (
    <MenuPrimitive.Item
      className={cn(
        "flex cursor-pointer select-none items-center gap-2 rounded-lg px-2 py-1.5 text-sm outline-none data-highlighted:bg-accent",
        className,
      )}
      data-slot="menu-item"
      {...props}
    />
  );
}

export function MenuSeparator({
  className,
  ...props
}: MenuPrimitive.Separator.Props): React.ReactElement {
  return (
    <MenuPrimitive.Separator
      className={cn("my-1 h-px bg-border", className)}
      data-slot="menu-separator"
      {...props}
    />
  );
}

export { MenuPopup as MenuContent };
