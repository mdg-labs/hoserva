// A catalog template's icon as an Avatar with a fallback (doc 03 §5.2,
// `p-avatar-1`): the icon is the API's, and an entry with no servable icon,
// which a template without an `icon` is, shows a neutral app glyph on the
// avatar's own tokens, so it reads the same in light and dark. The image
// renders only once it has loaded, so the glyph is all that shows until then.
import { Avatar as AvatarPrimitive } from "@base-ui/react/avatar";
import { Boxes } from "lucide-react";

import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { catalogIconPath } from "@/lib/api/operations";
import { cn } from "@/lib/utils";

export function TemplateIcon({
  id,
  className,
}: {
  id: string;
  className?: string;
}): React.ReactElement {
  return (
    <Avatar className={cn("size-12 rounded-lg", className)}>
      <AvatarPrimitive.Image
        src={catalogIconPath(id)}
        alt=""
        className="size-full object-contain"
      />
      <AvatarFallback aria-hidden="true">
        <Boxes className="size-1/2 text-muted-foreground" />
      </AvatarFallback>
    </Avatar>
  );
}
