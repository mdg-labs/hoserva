// `section-nav` (doc 03 "Shared patterns", `p-navigation-1`, `p-navigation-2`,
// `p-scroll-area-2`): segmented-styled links to a section's sub-routes
// (Storage, Apps, VMs, Settings, Tools), scrolling horizontally on mobile
// rather than wrapping.
import { Link, useLocation } from "react-router-dom";
import { useTranslation } from "react-i18next";

import { cn } from "@/lib/utils";
import { ScrollArea } from "@/components/ui/scroll-area";

export interface SectionNavItem {
  to: string;
  label: string;
}

export function SectionNav({ items }: { items: SectionNavItem[] }): React.ReactElement {
  const { pathname } = useLocation();
  const { t } = useTranslation();

  return (
    <ScrollArea className="max-w-full" scrollFade>
      <nav className="flex w-max gap-1 border-b pb-2" aria-label={t("sectionNav.ariaLabel")}>
        {items.map((item) => {
          const active = pathname === item.to;
          return (
            <Link
              key={item.to}
              to={item.to}
              aria-current={active ? "page" : undefined}
              className={cn(
                "rounded-md px-3 py-1.5 text-sm font-medium whitespace-nowrap transition-colors",
                active
                  ? "bg-secondary text-secondary-foreground"
                  : "text-muted-foreground hover:bg-secondary/50 hover:text-foreground",
              )}
            >
              {item.label}
            </Link>
          );
        })}
      </nav>
    </ScrollArea>
  );
}
