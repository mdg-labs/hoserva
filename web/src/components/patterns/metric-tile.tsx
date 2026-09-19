import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { Card, CardDescription, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Progress, ProgressIndicator, ProgressTrack } from "@/components/ui/progress";

export function MetricTile({
  title,
  value,
  description,
  footer,
  progress,
  to,
  onClick,
}: {
  title: ReactNode;
  value: ReactNode;
  description?: ReactNode;
  footer?: ReactNode;
  progress?: number | null;
  to?: string;
  onClick?: () => void;
}): React.ReactElement {
  const interactive = Boolean(to || onClick);
  const content = (
    <Card className={interactive ? "transition-colors hover:bg-muted/30" : undefined}>
      <CardHeader>
        <CardDescription>{title}</CardDescription>
        <CardTitle className="text-2xl">{value}</CardTitle>
        {description ? <p className="text-muted-foreground text-sm">{description}</p> : null}
      </CardHeader>
      {progress != null ? (
        <CardPanel>
          <Progress value={progress}>
            <ProgressTrack>
              <ProgressIndicator />
            </ProgressTrack>
          </Progress>
        </CardPanel>
      ) : null}
      {footer ? <CardPanel className="pt-0">{footer}</CardPanel> : null}
    </Card>
  );

  if (to) {
    return (
      <Link to={to} className="block no-underline text-inherit" onClick={onClick}>
        {content}
      </Link>
    );
  }

  if (onClick) {
    return (
      <button type="button" className="block w-full text-left" onClick={onClick}>
        {content}
      </button>
    );
  }

  return content;
}
