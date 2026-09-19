import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { Input } from "@/components/ui/input";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

const COMMON_TIMEZONES = [
  "UTC",
  "America/New_York",
  "America/Chicago",
  "America/Denver",
  "America/Los_Angeles",
  "Europe/London",
  "Europe/Berlin",
  "Europe/Paris",
  "Europe/Vienna",
  "Asia/Tokyo",
  "Australia/Sydney",
];

function listTimezones(): string[] {
  try {
    if (typeof Intl.supportedValuesOf === "function") {
      return Intl.supportedValuesOf("timeZone");
    }
  } catch {
    // Fall through to the short list on older runtimes.
  }
  return COMMON_TIMEZONES;
}

export function TimezoneSelect({
  value,
  onChange,
}: {
  value: string;
  onChange: (value: string) => void;
}): React.ReactElement {
  const { t } = useTranslation();
  const [query, setQuery] = useState("");

  const options = useMemo(() => {
    const detected = Intl.DateTimeFormat().resolvedOptions().timeZone;
    const merged = Array.from(
      new Set([value, detected, "UTC", ...listTimezones()].filter(Boolean)),
    );
    const needle = query.trim().toLowerCase();
    if (!needle) {
      return merged;
    }
    return merged.filter((zone) => zone.toLowerCase().includes(needle));
  }, [query, value]);

  return (
    <div className="flex flex-col gap-2">
      <Input
        value={query}
        onChange={(event) => setQuery(event.target.value)}
        placeholder={t("timezone.searchPlaceholder")}
      />
      <Select value={value} onValueChange={(next) => next && onChange(next)}>
        <SelectTrigger>
          <SelectValue placeholder={t("timezone.placeholder")} />
        </SelectTrigger>
        <SelectPopup>
          {options.map((zone) => (
            <SelectItem key={zone} value={zone}>
              {zone}
            </SelectItem>
          ))}
        </SelectPopup>
      </Select>
    </div>
  );
}
