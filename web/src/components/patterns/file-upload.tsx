// `file-upload` (doc 03 "Shared patterns", `p-input-5`): one file picked
// from the user's machine — the config restore archive, and later the
// Unraid Flash Backup and Compose import. It only holds the choice; the
// caller decides when the file is sent.
import type { ReactNode } from "react";

import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";

export function FileUpload({
  id,
  label,
  description,
  accept,
  disabled,
  onChange,
}: {
  id: string;
  label: ReactNode;
  description?: ReactNode;
  accept?: string;
  disabled?: boolean;
  onChange: (file: File | null) => void;
}): React.ReactElement {
  return (
    <Field>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <Input
        id={id}
        type="file"
        accept={accept}
        disabled={disabled}
        onChange={(event) => onChange(event.target.files?.[0] ?? null)}
      />
      {description ? <FieldDescription>{description}</FieldDescription> : null}
    </Field>
  );
}
