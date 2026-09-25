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
import { isApiError } from "@/lib/api/errors";
import { postAuthLogin } from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";

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

  const loginMutation = useApiMutation({
    mutationFn: async (body: { username: string; password: string; totpCode?: string }) => {
      const result = await postAuthLogin(body);
      if (result.error && isApiError(result.error)) {
        if (result.error.code === "totp_required") {
          setShowTotp(true);
          return { ...result, error: { ...result.error, message: t("login.errors.totpRequired") } };
        }
        if (result.error.code === "totp_invalid") {
          setShowTotp(true);
          return { ...result, error: { ...result.error, message: t("login.errors.totpInvalid") } };
        }
        if (result.error.code === "rate_limited") {
          return {
            ...result,
            error: { ...result.error, message: t("login.errors.rateLimited", { message: result.error.message }) },
          };
        }
      }
      return result;
    },
  });

  async function handleSubmit(event: React.FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    const result = await loginMutation.mutate({
      username: username.trim(),
      password,
      totpCode: showTotp && totpCode.length > 0 ? totpCode : undefined,
    });
    if (!result.ok) {
      return;
    }
    if (result.data) {
      acceptSession(result.data);
    }
    await refresh();
    navigate("/");
  }

  return (
    <Card className="mx-auto w-full max-w-md">
      <CardHeader>
        <CardTitle>{t("login.title")}</CardTitle>
        <CardDescription>{t("login.description")}</CardDescription>
      </CardHeader>
      <CardPanel>
        <form className="flex flex-col gap-4" onSubmit={(event) => void handleSubmit(event)}>
          {loginMutation.error ? <Banner tone="error" title={loginMutation.error} /> : null}
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
              <TotpInput value={totpCode} onChange={setTotpCode} aria-invalid={Boolean(loginMutation.error)} />
              <FieldDescription>{t("login.fields.totpHint")}</FieldDescription>
            </Field>
          ) : null}
          <Button type="submit" loading={loginMutation.pending} className="w-full">
            {t("login.submit")}
          </Button>
        </form>
      </CardPanel>
    </Card>
  );
}
