import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { SecretInput } from "@/components/patterns/secret-input";
import { TotpInput } from "@/components/patterns/totp";
import { Button } from "@/components/ui/button";
import { Card, CardDescription, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { useAuth } from "@/lib/api/auth-context";
import { hoservaClient } from "@/lib/api/client";
import { isApiError } from "@/lib/api/errors";

const USERNAME_AUTOCOMPLETE = "username";
const CURRENT_PASSWORD_AUTOCOMPLETE = "current-password";

export function LoginPage(): React.ReactElement {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { refresh, acceptSession } = useAuth();

  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [totpCode, setTotpCode] = useState("");
  const [showTotp, setShowTotp] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  async function handleSubmit(event: React.FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    setError(null);
    setLoading(true);
    try {
      const { data, error: apiError } = await hoservaClient.POST("/auth/login", {
        body: {
          username: username.trim(),
          password,
          totpCode: showTotp && totpCode.length > 0 ? totpCode : undefined,
        },
      });
      if (apiError) {
        if (isApiError(apiError)) {
          if (apiError.code === "totp_required") {
            setShowTotp(true);
            setError(t("login.errors.totpRequired"));
            return;
          }
          if (apiError.code === "totp_invalid") {
            setShowTotp(true);
            setError(t("login.errors.totpInvalid"));
            return;
          }
          if (apiError.code === "rate_limited") {
            setError(t("login.errors.rateLimited", { message: apiError.message }));
            return;
          }
        }
        setError(apiError.message);
        return;
      }
      if (data) {
        acceptSession(data);
      }
      await refresh();
      navigate("/");
    } finally {
      setLoading(false);
    }
  }

  return (
    <Card className="mx-auto w-full max-w-md">
      <CardHeader>
        <CardTitle>{t("login.title")}</CardTitle>
        <CardDescription>{t("login.description")}</CardDescription>
      </CardHeader>
      <CardPanel>
        <form className="flex flex-col gap-4" onSubmit={(event) => void handleSubmit(event)}>
          {error ? <Banner tone="error" title={error} /> : null}
          <Field>
            <FieldLabel>{t("login.fields.username")}</FieldLabel>
            <Input
              value={username}
              onChange={(event) => setUsername(event.target.value)}
              autoComplete={USERNAME_AUTOCOMPLETE}
              required
            />
          </Field>
          <Field>
            <FieldLabel>{t("login.fields.password")}</FieldLabel>
            <SecretInput
              value={password}
              onChange={setPassword}
              autoComplete={CURRENT_PASSWORD_AUTOCOMPLETE}
            />
          </Field>
          {showTotp ? (
            <Field>
              <FieldLabel>{t("login.fields.totp")}</FieldLabel>
              <TotpInput value={totpCode} onChange={setTotpCode} aria-invalid={Boolean(error)} />
              <FieldDescription>{t("login.fields.totpHint")}</FieldDescription>
            </Field>
          ) : null}
          <Button type="submit" loading={loading} className="w-full">
            {t("login.submit")}
          </Button>
        </form>
      </CardPanel>
    </Card>
  );
}
