import type { ReactNode } from "react";

import { Badge } from "@/components/ui/badge";
import {
  Collapsible,
  CollapsiblePanel,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";

export interface ResultGroup {
  id: string;
  label: ReactNode;
  count: number;
  items: ReactNode[];
  defaultOpen?: boolean;
}

export function GroupedResults({ groups }: { groups: ResultGroup[] }): React.ReactElement {
  return (
    <div className="flex flex-col gap-2">
      {groups.map((group) => (
        <Collapsible key={group.id} defaultOpen={group.defaultOpen ?? false}>
          <CollapsibleTrigger className="w-full">
            <span className="flex items-center gap-2">
              <span>{group.label}</span>
              <Badge variant="outline">{group.count}</Badge>
            </span>
          </CollapsibleTrigger>
          <CollapsiblePanel>
            <ul className="mt-2 space-y-1 ps-4 text-sm text-muted-foreground">
              {group.items.map((item, index) => (
                <li key={index}>{item}</li>
              ))}
            </ul>
          </CollapsiblePanel>
        </Collapsible>
      ))}
    </div>
  );
}
