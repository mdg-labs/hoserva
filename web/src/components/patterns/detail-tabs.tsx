import type { ReactNode } from "react";

import { Tabs, TabsList, TabsPanel, TabsTab } from "@/components/ui/tabs";

export interface DetailTab {
  id: string;
  label: ReactNode;
  content: ReactNode;
}

export function DetailTabs({
  tabs,
  defaultTab,
}: {
  tabs: DetailTab[];
  defaultTab?: string;
}): React.ReactElement {
  const initial = defaultTab ?? tabs[0]?.id ?? "";

  return (
    <Tabs defaultValue={initial}>
      <TabsList>
        {tabs.map((tab) => (
          <TabsTab key={tab.id} value={tab.id}>{tab.label}</TabsTab>
        ))}
      </TabsList>
      {tabs.map((tab) => (
        <TabsPanel key={tab.id} value={tab.id}>{tab.content}</TabsPanel>
      ))}
    </Tabs>
  );
}
