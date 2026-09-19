import { useTranslation } from "react-i18next";

import { OTPField, OTPFieldInput, OTPFieldSeparator } from "@/components/ui/otp-field";

export function TotpInput({
  value,
  onChange,
  disabled = false,
  "aria-invalid": ariaInvalid,
}: {
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  "aria-invalid"?: boolean;
}): React.ReactElement {
  const { t } = useTranslation();

  return (
    <OTPField
      length={6}
      value={value}
      onValueChange={onChange}
      disabled={disabled}
      aria-label={t("totp.label")}
    >
      <OTPFieldInput aria-invalid={ariaInvalid} />
      <OTPFieldInput aria-invalid={ariaInvalid} />
      <OTPFieldInput aria-invalid={ariaInvalid} />
      <OTPFieldSeparator />
      <OTPFieldInput aria-invalid={ariaInvalid} />
      <OTPFieldInput aria-invalid={ariaInvalid} />
      <OTPFieldInput aria-invalid={ariaInvalid} />
    </OTPField>
  );
}
