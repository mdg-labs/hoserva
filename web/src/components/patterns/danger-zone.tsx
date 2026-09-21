// `danger-zone` (doc 03 "Shared patterns", `p-frame-1`, `p-button-5`):
// remove disk, delete share, remove app or VM — every irreversible action
// lives here, each leading to its own `confirm` or `typed-confirm`
// dialog rather than acting immediately.
import type { ReactNode } from "react";

import { Button } from "@/components/ui/button";
import { Frame, FramePanel } from "@/components/ui/frame";

export interface DangerZoneAction {
  id: string;
  title: ReactNode;
  description: ReactNode;
  actionLabel: ReactNode;
  onAction: () => void;
  disabled?: boolean;
}

export function DangerZone({ actions }: { actions: DangerZoneAction[] }): React.ReactElement {
  return (
    <Frame>
      {actions.map((action) => (
        <FramePanel key={action.id} className="flex flex-wrap items-start justify-between gap-4">
          <div className="flex flex-col gap-1">
            <p className="font-medium">{action.title}</p>
            <p className="text-muted-foreground text-sm">{action.description}</p>
          </div>
          <Button
            type="button"
            variant="destructive-outline"
            disabled={action.disabled}
            onClick={action.onAction}
          >
            {action.actionLabel}
          </Button>
        </FramePanel>
      ))}
    </Frame>
  );
}
