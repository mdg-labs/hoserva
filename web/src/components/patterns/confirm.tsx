import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
} from "@/components/ui/dialog";

export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  error,
  items,
  confirmLabel,
  cancelLabel,
  onConfirm,
  loading = false,
  destructive = false,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  description?: ReactNode;
  error?: ReactNode;
  items?: ReactNode[];
  confirmLabel?: ReactNode;
  cancelLabel?: ReactNode;
  onConfirm: () => void;
  loading?: boolean;
  destructive?: boolean;
}): React.ReactElement {
  const { t } = useTranslation();

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogPopup>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          {description ? <DialogDescription>{description}</DialogDescription> : null}
        </DialogHeader>
        <DialogPanel>
          {error ? <Banner tone="error" title={error} /> : null}
          {items && items.length > 0 ? (
            <ul className="list-disc space-y-1 ps-5 text-sm">
              {items.map((item, index) => (
                <li key={index}>{item}</li>
              ))}
            </ul>
          ) : null}
        </DialogPanel>
        <DialogFooter>
          <DialogClose render={<Button variant="outline" />}>
            {cancelLabel ?? t("confirm.cancel")}
          </DialogClose>
          <Button
            variant={destructive ? "destructive" : "default"}
            loading={loading}
            onClick={() => {
              onConfirm();
            }}
          >
            {confirmLabel ?? t("confirm.confirm")}
          </Button>
        </DialogFooter>
      </DialogPopup>
    </Dialog>
  );
}
