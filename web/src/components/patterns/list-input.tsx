// `list-input` (doc 03 "Shared patterns", `p-group-16`, `p-badge-20`):
// NTP servers, NFS hosts — a short free-text list edited as removable
// chips rather than a comma-separated field.
import { Plus, X } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

export function ListInput({
  value,
  onChange,
  placeholder,
  disabled = false,
}: {
  value: string[];
  onChange: (value: string[]) => void;
  placeholder?: string;
  disabled?: boolean;
}): React.ReactElement {
  const { t } = useTranslation();
  const [draft, setDraft] = useState("");

  function add(): void {
    const trimmed = draft.trim();
    if (trimmed.length === 0 || value.includes(trimmed)) {
      return;
    }
    onChange([...value, trimmed]);
    setDraft("");
  }

  function remove(entry: string): void {
    onChange(value.filter((item) => item !== entry));
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="flex gap-2">
        <Input
          value={draft}
          disabled={disabled}
          placeholder={placeholder}
          onChange={(event) => setDraft(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter") {
              event.preventDefault();
              add();
            }
          }}
        />
        <Button type="button" variant="outline" size="icon" disabled={disabled} onClick={add}>
          <Plus aria-hidden="true" />
          <span className="sr-only">{t("listInput.add")}</span>
        </Button>
      </div>
      {value.length > 0 ? (
        <div className="flex flex-wrap gap-1.5">
          {value.map((entry) => (
            <Badge key={entry} variant="outline" className="gap-1 py-1">
              {entry}
              <button
                type="button"
                disabled={disabled}
                aria-label={t("listInput.remove", { value: entry })}
                onClick={() => remove(entry)}
                className="rounded-sm opacity-70 outline-none hover:opacity-100 focus-visible:ring-2 focus-visible:ring-ring"
              >
                <X aria-hidden="true" className="size-3" />
              </button>
            </Badge>
          ))}
        </div>
      ) : null}
    </div>
  );
}
