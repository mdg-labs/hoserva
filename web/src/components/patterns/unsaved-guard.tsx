import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogPopup,
  DialogTitle,
} from "@/components/ui/dialog";

export function UnsavedGuardDialog({
  open,
  onDiscard,
  onStay,
}: {
  open: boolean;
  onDiscard: () => void;
  onStay: () => void;
}): React.ReactElement {
  const { t } = useTranslation();

  return (
    <Dialog open={open} onOpenChange={(next) => !next && onStay()}>
      <DialogPopup showCloseButton={false}>
        <DialogHeader>
          <DialogTitle>{t("unsavedGuard.title")}</DialogTitle>
          <DialogDescription>{t("unsavedGuard.description")}</DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <DialogClose render={<Button variant="outline" onClick={onStay} />}>
            {t("unsavedGuard.stay")}
          </DialogClose>
          <Button variant="destructive" onClick={onDiscard}>
            {t("unsavedGuard.discard")}
          </Button>
        </DialogFooter>
      </DialogPopup>
    </Dialog>
  );
}
