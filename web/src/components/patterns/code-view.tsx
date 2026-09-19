import { ChevronDownIcon } from "lucide-react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { Collapsible, CollapsiblePanel, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Frame, FramePanel } from "@/components/ui/frame";
import { ScrollArea } from "@/components/ui/scroll-area";
import { cn } from "@/lib/utils";

export function CodeView({
  title,
  code,
  defaultOpen = false,
}: {
  title: ReactNode;
  code: string;
  defaultOpen?: boolean;
}): React.ReactElement {
  const { t } = useTranslation();

  return (
    <Collapsible defaultOpen={defaultOpen}>
      <CollapsibleTrigger>
        <span>{title}</span>
        <ChevronDownIcon
          aria-hidden="true"
          className={cn("size-4 text-muted-foreground transition-transform [[data-panel-open]_&]:rotate-180")}
        />
      </CollapsibleTrigger>
      <CollapsiblePanel>
        <Frame className="mt-2">
          <FramePanel className="p-0">
            <ScrollArea className="max-h-80">
              <pre className="overflow-x-auto p-4 font-mono text-xs leading-relaxed">
                <code>{code}</code>
              </pre>
            </ScrollArea>
          </FramePanel>
        </Frame>
        <span className="sr-only">{t("codeView.readOnly")}</span>
      </CollapsiblePanel>
    </Collapsible>
  );
}
