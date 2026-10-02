// `path-picker` (doc 03 "Shared patterns"): a path typed by hand or chosen
// from the locations the API offers, such as the existing shares. The
// choices are the caller's: nothing here looks at the file system or decides
// what a good default is.
import { FolderOpen } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group";
import { Menu, MenuContent, MenuItem, MenuTrigger } from "@/components/ui/menu";

const END_ALIGN = "inline-end" as const;
const AUTOCOMPLETE_OFF = "off";

export function PathPicker({
  id,
  value,
  onChange,
  suggestions = [],
  "aria-invalid": ariaInvalid,
}: {
  id?: string;
  value: string;
  onChange: (value: string) => void;
  suggestions?: string[];
  "aria-invalid"?: boolean;
}): React.ReactElement {
  const { t } = useTranslation();

  return (
    <InputGroup>
      <InputGroupInput
        id={id}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        aria-invalid={ariaInvalid}
        autoComplete={AUTOCOMPLETE_OFF}
        spellCheck={false}
        className="font-mono"
      />
      {suggestions.length > 0 ? (
        <InputGroupAddon align={END_ALIGN}>
          <Menu>
            <MenuTrigger
              render={
                <Button type="button" size="icon-xs" variant="ghost" aria-label={t("pathPicker.choose")}>
                  <FolderOpen aria-hidden="true" />
                </Button>
              }
            />
            <MenuContent>
              {suggestions.map((suggestion) => (
                <MenuItem key={suggestion} onClick={() => onChange(suggestion)}>
                  <span className="font-mono text-xs">{suggestion}</span>
                </MenuItem>
              ))}
            </MenuContent>
          </Menu>
        </InputGroupAddon>
      ) : null}
    </InputGroup>
  );
}
