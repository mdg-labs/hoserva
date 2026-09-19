import type { ReactNode } from "react";

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
  return (
    <InputGroup>
      <InputGroupInput
        render={
          <Input
            id={id}
            type="number"
            inputMode={NUMERIC_INPUT_MODE}
            value={value}
            min={min}
            max={max}
            disabled={disabled}
            onChange={(event) => {
              const next = Number.parseInt(event.target.value, 10);
              if (!Number.isNaN(next)) {
                onChange(next);
              }
            }}
          />
        }
      />
      <InputGroupAddon align={END_ALIGN}>{unit}</InputGroupAddon>
    </InputGroup>
  );
}
