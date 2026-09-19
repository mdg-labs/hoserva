import { Eye, EyeOff, RefreshCw } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group";
import { Progress, ProgressIndicator, ProgressTrack } from "@/components/ui/progress";
import { cn } from "@/lib/utils";

const END_ALIGN = "inline-end" as const;

export type SecretStrength = "weak" | "fair" | "good" | "strong";

function scorePassword(value: string): { strength: SecretStrength; score: number } {
  let score = 0;
  if (value.length >= 12) score += 1;
  if (value.length >= 16) score += 1;
  if (/[a-z]/.test(value) && /[A-Z]/.test(value)) score += 1;
  if (/\d/.test(value)) score += 1;
  if (/[^A-Za-z0-9]/.test(value)) score += 1;
  if (score <= 2) return { strength: "weak", score: 25 };
  if (score === 3) return { strength: "fair", score: 50 };
  if (score === 4) return { strength: "good", score: 75 };
  return { strength: "strong", score: 100 };
}

function generatePassword(): string {
  const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789!@#$%^&*";
  const bytes = crypto.getRandomValues(new Uint8Array(20));
  return Array.from(bytes, (byte) => alphabet[byte % alphabet.length]).join("");
}

export function SecretInput({
  id,
  value,
  onChange,
  showStrength = false,
  showGenerate = false,
  placeholder,
  autoComplete,
  "aria-invalid": ariaInvalid,
}: {
  id?: string;
  value: string;
  onChange: (value: string) => void;
  showStrength?: boolean;
  showGenerate?: boolean;
  placeholder?: string;
  autoComplete?: string;
  "aria-invalid"?: boolean;
}): React.ReactElement {
  const { t } = useTranslation();
  const [visible, setVisible] = useState(false);
  const strength = useMemo(() => scorePassword(value), [value]);

  return (
    <div className="flex w-full flex-col gap-2">
      <InputGroup>
        <InputGroupInput
          id={id}
          type={visible ? "text" : "password"}
          value={value}
          onChange={(event) => onChange(event.target.value)}
          placeholder={placeholder}
          autoComplete={autoComplete}
          aria-invalid={ariaInvalid}
        />
        <InputGroupAddon align={END_ALIGN} className="flex gap-0.5">
          {showGenerate ? (
            <Button
              type="button"
              size="icon-xs"
              variant="ghost"
              aria-label={t("secretInput.generate")}
              onClick={() => onChange(generatePassword())}
            >
              <RefreshCw aria-hidden="true" />
            </Button>
          ) : null}
          <Button
            type="button"
            size="icon-xs"
            variant="ghost"
            aria-label={visible ? t("secretInput.hide") : t("secretInput.show")}
            onClick={() => setVisible((current) => !current)}
          >
            {visible ? <EyeOff aria-hidden="true" /> : <Eye aria-hidden="true" />}
          </Button>
        </InputGroupAddon>
      </InputGroup>
      {showStrength && value.length > 0 ? (
        <div className="flex flex-col gap-1">
          <Progress value={strength.score}>
            <ProgressTrack className="h-1">
              <ProgressIndicator
                className={cn(
                  strength.strength === "weak" && "bg-destructive",
                  strength.strength === "fair" && "bg-warning",
                  strength.strength === "good" && "bg-info",
                  strength.strength === "strong" && "bg-success",
                )}
              />
            </ProgressTrack>
          </Progress>
          <p className="text-muted-foreground text-xs">{t(`secretInput.strength.${strength.strength}`)}</p>
        </div>
      ) : null}
    </div>
  );
}
