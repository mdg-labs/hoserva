import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";

export function TypedConfirm({
  phrase,
  value,
  onChange,
  title,
  description,
  items,
}: {
  phrase: string;
  value: string;
  onChange: (value: string) => void;
  title: ReactNode;
  description?: ReactNode;
  items: ReactNode[];
}): React.ReactElement {
  const { t } = useTranslation();
  const matches = value === phrase;

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h3 className="font-medium">{title}</h3>
        {description ? <p className="text-muted-foreground mt-1 text-sm">{description}</p> : null}
      </div>
      <ul className="list-disc space-y-1 ps-5 text-sm">
        {items.map((item, index) => (
          <li key={index}>{item}</li>
        ))}
      </ul>
      <Field>
        <FieldLabel>{t("typedConfirm.label")}</FieldLabel>
        <Input value={value} onChange={(event) => onChange(event.target.value)} />
        <FieldDescription>{t("typedConfirm.hint", { phrase })}</FieldDescription>
        {value.length > 0 && !matches ? <FieldError>{t("typedConfirm.mismatch")}</FieldError> : null}
      </Field>
    </div>
  );
}

