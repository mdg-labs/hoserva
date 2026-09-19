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
    const merged = Array.from(new Set([detected, ...COMMON_TIMEZONES].filter(Boolean)));
    const needle = query.trim().toLowerCase();
    if (!needle) {
      return merged;
    }
    return merged.filter((zone) => zone.toLowerCase().includes(needle));
  }, [query]);

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
