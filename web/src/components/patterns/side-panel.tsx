// `side-panel` (doc 03 "Shared patterns", `p-sheet-1`): editing or
// inspecting one row while the list behind it stays visible — the create
// and edit account panel on `/users` (doc 03 §7).
import type { ReactNode } from "react";

import {
  Sheet,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetPanel,
  SheetPopup,
  SheetTitle,
} from "@/components/ui/sheet";

export function SidePanel({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  description?: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
}): React.ReactElement {
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetPopup>
        <SheetHeader>
          <SheetTitle>{title}</SheetTitle>
          {description ? <SheetDescription>{description}</SheetDescription> : null}
        </SheetHeader>
        <SheetPanel className="flex flex-col gap-4">{children}</SheetPanel>
        {footer ? <SheetFooter>{footer}</SheetFooter> : null}
      </SheetPopup>
    </Sheet>
  );
}
