import { Check, Copy } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group";

const END_ALIGN = "inline-end" as const;

export function CopyValue({ value, label }: { value: string; label?: string }): React.ReactElement {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const [copyError, setCopyError] = useState(false);

  async function handleCopy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      setCopyError(false);
      window.setTimeout(() => setCopied(false), 2000);
    } catch {
      setCopied(false);
      setCopyError(true);
    }
  }

  return (
    <div className="flex flex-col gap-1">
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
      {copyError ? <p className="text-destructive text-sm">{t("copyValue.failed")}</p> : null}
    </div>
  );
}
