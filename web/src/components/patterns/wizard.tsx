import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Card, CardFooter, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Progress, ProgressIndicator, ProgressTrack } from "@/components/ui/progress";

export function Wizard({
  step,
  stepCount,
  title,
  description,
  children,
  onBack,
  onNext,
  nextLabel,
  backDisabled = false,
  nextDisabled = false,
  nextLoading = false,
}: {
  step: number;
  stepCount: number;
  title: string;
  description?: string;
  children: ReactNode;
  onBack?: () => void;
  onNext: () => void;
  nextLabel?: string;
  backDisabled?: boolean;
  nextDisabled?: boolean;
  nextLoading?: boolean;
}): React.ReactElement {
  const { t } = useTranslation();
  const progress = ((step + 1) / stepCount) * 100;

  return (
    <Card className="mx-auto w-full max-w-2xl">
      <CardHeader>
        <p className="text-muted-foreground text-sm">
          {t("wizard.stepProgress", { current: step + 1, total: stepCount })}
        </p>
        <Progress value={progress}>
          <ProgressTrack>
            <ProgressIndicator />
          </ProgressTrack>
        </Progress>
        <CardTitle>{title}</CardTitle>
        {description ? <p className="text-muted-foreground text-sm">{description}</p> : null}
      </CardHeader>
      <CardPanel>{children}</CardPanel>
      <CardFooter className="justify-between gap-3">
        {onBack ? (
          <Button type="button" variant="outline" disabled={backDisabled} onClick={onBack}>
            {t("wizard.back")}
          </Button>
        ) : (
          <span />
        )}
        <Button type="button" loading={nextLoading} disabled={nextDisabled} onClick={onNext}>
          {nextLabel ?? t("wizard.next")}
        </Button>
      </CardFooter>
    </Card>
  );
}
