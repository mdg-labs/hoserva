import { useState, type ReactNode } from "react";

import { UnsavedGuardDialog } from "@/components/patterns/unsaved-guard";

export function useUnsavedGuard({
  dirty,
  onClose,
}: {
  dirty: boolean;
  onClose: () => void;
}): {
  requestClose: () => void;
  guardDialog: ReactNode;
} {
  const [pending, setPending] = useState(false);

  const requestClose = (): void => {
    if (dirty) {
      setPending(true);
      return;
    }
    onClose();
  };

  const guardDialog = (
    <UnsavedGuardDialog
      open={pending}
      onStay={() => setPending(false)}
      onDiscard={() => {
        setPending(false);
        onClose();
      }}
    />
  );

  return { requestClose, guardDialog };
}
