// `multi-pick` (doc 03 "Shared patterns", `p-combobox-19`): groups,
// systemd units, categories — a searchable multi-select with removable
// chips for the current selection.
import { useTranslation } from "react-i18next";

import {
  Combobox,
  ComboboxChip,
  ComboboxChipRemove,
  ComboboxChips,
  ComboboxEmpty,
  ComboboxInput,
  ComboboxItem,
  ComboboxList,
  ComboboxPopup,
} from "@/components/ui/combobox";

export interface MultiPickOption {
  value: string;
  label: string;
}

export function MultiPick({
  value,
  onChange,
  options,
  placeholder,
  emptyLabel,
}: {
  value: string[];
  onChange: (value: string[]) => void;
  options: MultiPickOption[];
  placeholder?: string;
  emptyLabel?: string;
}): React.ReactElement {
  const { t } = useTranslation();
  const labelByValue = new Map(options.map((option) => [option.value, option.label]));

  return (
    <Combobox
      items={options}
      multiple
      value={value}
      onValueChange={(next) => onChange(next)}
    >
      <ComboboxChips>
        {value.map((entry) => (
          <ComboboxChip key={entry}>
            {labelByValue.get(entry) ?? entry}
            <ComboboxChipRemove aria-label={t("multiPick.remove", { value: labelByValue.get(entry) ?? entry })} />
          </ComboboxChip>
        ))}
        <ComboboxInput placeholder={value.length === 0 ? placeholder : undefined} />
      </ComboboxChips>
      <ComboboxPopup>
        <ComboboxEmpty>{emptyLabel ?? t("multiPick.empty")}</ComboboxEmpty>
        <ComboboxList>
          {(option: MultiPickOption) => (
            <ComboboxItem key={option.value} value={option.value}>
              {option.label}
            </ComboboxItem>
          )}
        </ComboboxList>
      </ComboboxPopup>
    </Combobox>
  );
}
