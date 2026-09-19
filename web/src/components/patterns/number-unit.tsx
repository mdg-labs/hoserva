import { useState, type ReactNode } from "react";

import { Input } from "@/components/ui/input";
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group";

const NUMERIC_INPUT_MODE = "numeric";
const END_ALIGN = "inline-end";

export function NumberUnit({
  id,
  value,
  onChange,
  unit,
  min,
  max,
  disabled = false,
}: {
  id?: string;
  value: number;
  onChange: (value: number) => void;
  unit: ReactNode;
  min?: number;
  max?: number;
  disabled?: boolean;
}): React.ReactElement {
  const [draft, setDraft] = useState<string | null>(null);

  return (
    <InputGroup>
      <InputGroupInput
        render={
          <Input
            id={id}
            type="number"
            inputMode={NUMERIC_INPUT_MODE}
            value={draft ?? String(value)}
            min={min}
            max={max}
            disabled={disabled}
            onChange={(event) => {
              const raw = event.target.value;
              setDraft(raw);
              if (raw === "") {
                return;
              }
              const next = Number.parseInt(raw, 10);
              if (!Number.isNaN(next)) {
                onChange(next);
              }
            }}
            onBlur={() => setDraft(null)}
          />
        }
      />
      <InputGroupAddon align={END_ALIGN}>{unit}</InputGroupAddon>
    </InputGroup>
  );
}
