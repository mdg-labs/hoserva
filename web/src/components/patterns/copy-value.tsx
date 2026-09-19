import { Check, Copy } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group";

const END_ALIGN = "inline-end" as const;

export function CopyValue({ value, label }: { value: string; label?: string }): React.ReactElement {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);

  async function handleCopy(): Promise<void> {
    await navigator.clipboard.writeText(value);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 2000);
  }

  return (
    <InputGroup>
      <InputGroupInput readOnly value={value} aria-label={label ?? t("copyValue.label")} />
      <InputGroupAddon align={END_ALIGN}>
        <Button
          type="button"
          size="icon-xs"
          variant="ghost"
          aria-label={copied ? t("copyValue.copied") : t("copyValue.copy")}
          onClick={() => void handleCopy()}
        >
          {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
        </Button>
      </InputGroupAddon>
    </InputGroup>
  );
}
