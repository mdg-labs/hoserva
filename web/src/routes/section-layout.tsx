import { Outlet } from "react-router-dom";

import { SectionNav, type SectionNavItem } from "@/components/patterns/section-nav";

export function SectionLayout({ items }: { items: SectionNavItem[] }): React.ReactElement {
  return (
    <div className="flex flex-col gap-4">
      <SectionNav items={items} />
      <Outlet />
    </div>
  );
}
