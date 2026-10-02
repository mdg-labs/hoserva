// A catalog template's icon as an Avatar with a fallback (doc 03 §5.2,
// `p-avatar-1`): the icon is the API's, and an entry with no servable icon
// shows the first letter of its title instead.
import { Avatar as AvatarPrimitive } from "@base-ui/react/avatar";

import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { catalogIconPath } from "@/lib/api/operations";
import { cn } from "@/lib/utils";

export function TemplateIcon({
  id,
  title,
  className,
}: {
  id: string;
  title: string;
  className?: string;
}): React.ReactElement {
  return (
    <Avatar className={cn("size-12 rounded-lg", className)}>
      <AvatarPrimitive.Image
        src={catalogIconPath(id)}
        alt=""
        className="size-full object-contain"
      />
      <AvatarFallback aria-hidden="true">{title.slice(0, 1).toUpperCase()}</AvatarFallback>
    </Avatar>
  );
}
