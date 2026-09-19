import type { ReactNode } from "react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Info } from "lucide-react";

export function InlineNote({
  title,
  description,
}: {
  title?: ReactNode;
  description: ReactNode;
}): React.ReactElement {
  return (
    <Alert variant="info">
      <Info aria-hidden="true" />
      {title ? <AlertTitle>{title}</AlertTitle> : null}
      <AlertDescription>{description}</AlertDescription>
    </Alert>
  );
}
