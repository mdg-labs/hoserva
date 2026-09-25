import {
  Bell,
  HardDrive,
  Mail,
  MessageSquare,
  Server,
  Webhook,
} from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import { CopyValue } from "@/components/patterns/copy-value";
import { Banner } from "@/components/patterns/banner";
import { ChoiceCards } from "@/components/patterns/choice-cards";
import { SecretInput } from "@/components/patterns/secret-input";
import { doctorBlocksProgress } from "@/components/patterns/doctor-checks";
import { StackedChecks } from "@/components/patterns/stacked-checks";
import { TimezoneSelect } from "@/components/patterns/timezone";
import { TotpInput } from "@/components/patterns/totp";
import { Wizard } from "@/components/patterns/wizard";
import { Button } from "@/components/ui/button";
import { Card, CardDescription, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useAuth } from "@/lib/api/auth-context";
import type { components } from "@/lib/api/client";
import {
  markOnboardingComplete,
  markOnboardingIncomplete,
  readOnboardingStep,
  writeOnboardingStep,
} from "@/lib/api/onboarding";
import {
  getDoctor,
  postAuthTotpConfirm,
  postAuthTotpEnroll,
  postDoctorHostConfig,
  postNotificationChannel,
  postSetupAdmin,
  putGeneralSettings,
} from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";

type DoctorCheck = components["schemas"]["DoctorCheck"];
type NotificationChannelType = components["schemas"]["NotificationChannelType"];

const STEP_COUNT = 4;
const PATH_FRESH = "fresh";
const PATH_MIGRATE = "migrate";
const Q76_IMPORT = "import";
const Q76_LEAVE = "leave";
const ONBOARDING_PATH_NAME = "onboarding-path";
const USERNAME_AUTOCOMPLETE = "username";
const NEW_PASSWORD_AUTOCOMPLETE = "new-password";

const Q76_CATEGORIES = [
  { id: "host_samba", labelKey: "welcome.q76.samba" },
  { id: "host_nfs", labelKey: "welcome.q76.nfs" },
  { id: "host_fstab", labelKey: "welcome.q76.fstab" },
  { id: "host_docker_containers", labelKey: "welcome.q76.dockerContainers" },
  { id: "host_docker_images", labelKey: "welcome.q76.dockerImages" },
] as const;

const CHANNEL_ICONS: Record<NotificationChannelType, typeof Mail> = {
  discord: MessageSquare,
  email: Mail,
  gotify: Bell,
  ntfy: Bell,
  webhook: Webhook,
};

function findQ76Check(checks: DoctorCheck[], id: string): DoctorCheck | undefined {
  return checks.find((check) => check.id === id || check.id.startsWith(`${id}_`));
}

export function WelcomePage(): React.ReactElement {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { refresh, acceptSession } = useAuth();

  const [step, setStep] = useState(() => readOnboardingStep());
  const [error, setError] = useState<string | null>(null);

  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [enableTotp, setEnableTotp] = useState(false);
  const [adminCreated, setAdminCreated] = useState(false);
  const [totpSecret, setTotpSecret] = useState<string | null>(null);
  const [otpauthUri, setOtpauthUri] = useState<string | null>(null);
  const [totpConfirmCode, setTotpConfirmCode] = useState("");

  const doctorQuery = useApiQuery<{ checks: DoctorCheck[] }>({
    queryKey: "welcome-doctor",
    queryFn: (signal) => getDoctor(signal),
    enabled: step === 1,
    fallbackError: t("welcome.errors.doctorLoadFailed"),
  });
  const doctorChecks = doctorQuery.data?.checks ?? null;
  const [q76Choices, setQ76Choices] = useState<Record<string, "import" | "leave">>({});

  // Every welcome-flow mutation goes through useApiMutation so a dropped
  // connection during admin creation, TOTP enrol/confirm, the doctor
  // host-config decision or the basics save surfaces as an error instead of
  // an unhandled rejection (issue #271 finding 1).
  const setupAdminMutation = useApiMutation<
    { username: string; password: string },
    components["schemas"]["User"]
  >({
    mutationFn: postSetupAdmin,
  });
  const totpEnrollMutation = useApiMutation<undefined, { secret: string; otpauthUri: string }>({
    mutationFn: () => postAuthTotpEnroll(),
  });
  const totpConfirmMutation = useApiMutation<string, unknown>({
    mutationFn: (code) => postAuthTotpConfirm(code),
  });
  const doctorHostConfigMutation = useApiMutation<components["schemas"]["ApplyHostConfigRequest"], unknown>({
    mutationFn: postDoctorHostConfig,
  });
  const notificationChannelMutation = useApiMutation<
    { name: string; type: NotificationChannelType; enabled: boolean; secret?: string },
    unknown
  >({
    mutationFn: postNotificationChannel,
  });
  const generalSettingsMutation = useApiMutation<
    components["schemas"]["UpdateGeneralSettingsRequest"],
    unknown
  >({
    mutationFn: putGeneralSettings,
  });

  // Set at each step handler's entry and cleared in `finally`, so Next
  // stays disabled across the whole handler — not just while a mutation's
  // own `pending` flag is true. handleAdminStep awaits `refresh()` after
  // setupAdminMutation resolves; without this, `loading` would drop back to
  // false during that gap and a second Next click would re-post
  // /setup/admin before `adminCreated` is set (issue #271 finding).
  const [stepBusy, setStepBusy] = useState(false);

  const loading =
    stepBusy ||
    setupAdminMutation.pending ||
    totpEnrollMutation.pending ||
    totpConfirmMutation.pending ||
    doctorHostConfigMutation.pending ||
    notificationChannelMutation.pending ||
    generalSettingsMutation.pending;

  const [hostname, setHostname] = useState("");
  const [timezone, setTimezone] = useState(() => Intl.DateTimeFormat().resolvedOptions().timeZone);
  const [channelType, setChannelType] = useState<NotificationChannelType>("email");
  const [channelName, setChannelName] = useState("");
  const [channelSecret, setChannelSecret] = useState("");
  const [skipNotification, setSkipNotification] = useState(false);
  const [backupPassphrase, setBackupPassphrase] = useState("");
  const [skipBackupPassphrase, setSkipBackupPassphrase] = useState(false);

  const [pathChoice, setPathChoice] = useState(PATH_FRESH);

  useEffect(() => {
    writeOnboardingStep(step);
  }, [step]);

  const q76Panels = useMemo(
    () =>
      Q76_CATEGORIES.map((category) => ({
        ...category,
        check: doctorChecks ? findQ76Check(doctorChecks, category.id) : undefined,
      })),
    [doctorChecks],
  );

  const stepMeta = [
    {
      title: t("welcome.steps.admin.title"),
      description: t("welcome.steps.admin.description"),
    },
    {
      title: t("welcome.steps.doctor.title"),
      description: t("welcome.steps.doctor.description"),
    },
    {
      title: t("welcome.steps.basics.title"),
      description: t("welcome.steps.basics.description"),
    },
    {
      title: t("welcome.steps.path.title"),
      description: t("welcome.steps.path.description"),
    },
  ][step];

  async function enrollTotp(): Promise<boolean> {
    const result = await totpEnrollMutation.mutate(undefined);
    if (!result.ok) {
      if (!result.aborted) setError(result.error);
      return false;
    }
    if (!result.data) {
      setError(t("welcome.errors.totpEnrollFailed"));
      return false;
    }
    setTotpSecret(result.data.secret);
    setOtpauthUri(result.data.otpauthUri);
    return true;
  }

  async function handleAdminStep(): Promise<void> {
    setStepBusy(true);
    try {
      if (adminCreated && enableTotp) {
        if (!totpSecret) {
          setError(null);
          await enrollTotp();
          return;
        }
        if (totpConfirmCode.length < 6) {
          setError(t("welcome.errors.totpConfirmRequired"));
          return;
        }
        setError(null);
        const result = await totpConfirmMutation.mutate(totpConfirmCode);
        if (!result.ok) {
          if (!result.aborted) setError(result.error);
          return;
        }
        setStep(1);
        return;
      }

      if (username.trim().length === 0) {
        setError(t("welcome.errors.usernameRequired"));
        return;
      }
      if (password.length < 12) {
        setError(t("welcome.errors.passwordTooShort"));
        return;
      }
      setError(null);
      const result = await setupAdminMutation.mutate({ username: username.trim(), password });
      if (!result.ok) {
        if (!result.aborted) setError(result.error);
        return;
      }
      if (!result.data) {
        return;
      }
      markOnboardingIncomplete();
      acceptSession(result.data);
      await refresh();
      setAdminCreated(true);

      if (enableTotp) {
        await enrollTotp();
        return;
      }

      setStep(1);
    } finally {
      setStepBusy(false);
    }
  }

  async function handleDoctorNext(): Promise<void> {
    setStepBusy(true);
    setError(null);
    try {
      const files = q76Panels
        .filter((panel) => panel.check)
        .map((panel) => ({
          id: panel.id,
          decision: q76Choices[panel.id] ?? Q76_LEAVE,
        }));
      if (files.length > 0) {
        const result = await doctorHostConfigMutation.mutate({ files });
        if (!result.ok) {
          if (!result.aborted) setError(result.error);
          return;
        }
      }
      setStep(2);
    } finally {
      setStepBusy(false);
    }
  }

  async function handleBasicsNext(): Promise<void> {
    setStepBusy(true);
    setError(null);
    try {
      if (!skipNotification && channelName.trim().length > 0) {
        const channelResult = await notificationChannelMutation.mutate({
          name: channelName.trim(),
          type: channelType,
          enabled: true,
          secret: channelSecret || undefined,
        });
        if (!channelResult.ok) {
          if (!channelResult.aborted) setError(channelResult.error);
          return;
        }
      }

      const settingsBody: {
        hostname?: string;
        timezone?: string;
        backupPassphrase?: string;
      } = {
        timezone,
      };
      if (hostname.trim().length > 0) {
        settingsBody.hostname = hostname.trim();
      }
      if (!skipBackupPassphrase && backupPassphrase.length > 0) {
        settingsBody.backupPassphrase = backupPassphrase;
      }
      const settingsResult = await generalSettingsMutation.mutate(settingsBody);
      if (!settingsResult.ok) {
        if (!settingsResult.aborted) setError(settingsResult.error);
        return;
      }

      setStep(3);
    } finally {
      setStepBusy(false);
    }
  }

  function handleFinish(): void {
    markOnboardingComplete();
    void refresh();
    if (pathChoice === PATH_MIGRATE) {
      navigate("/tools/migrate");
      return;
    }
    navigate("/storage/setup");
  }

  function handleNext(): void {
    setError(null);
    if (step === 0) {
      void handleAdminStep();
      return;
    }
    if (step === 1) {
      if (doctorChecks && doctorBlocksProgress(doctorChecks)) {
        setError(t("welcome.errors.storageBlocked"));
        return;
      }
      void handleDoctorNext();
      return;
    }
    if (step === 2) {
      void handleBasicsNext();
      return;
    }
    handleFinish();
  }

  const nextDisabled =
    (step === 0 &&
      !adminCreated &&
      (username.trim().length === 0 || password.length < 12)) ||
    (step === 0 && adminCreated && enableTotp && Boolean(totpSecret) && totpConfirmCode.length < 6) ||
    (step === 1 && (doctorChecks === null || doctorBlocksProgress(doctorChecks))) ||
    (step === 3 && pathChoice.length === 0);

  const nextLabel =
    step === 0 && adminCreated && enableTotp && totpSecret
      ? t("welcome.confirmTotp")
      : step === STEP_COUNT - 1
        ? t("welcome.finish")
        : undefined;

  return (
    <Wizard
      step={step}
      stepCount={STEP_COUNT}
      title={stepMeta.title}
      description={stepMeta.description}
      onBack={step > 0 ? () => setStep(step - 1) : undefined}
      onNext={handleNext}
      nextDisabled={nextDisabled}
      nextLoading={loading}
      nextLabel={nextLabel}
    >
      {error ? <Banner tone="error" title={error} /> : null}

      {step === 0 ? (
        <div className="flex flex-col gap-4">
          <Field>
            <FieldLabel>{t("welcome.fields.username")}</FieldLabel>
            <Input
              value={username}
              onChange={(event) => setUsername(event.target.value)}
              autoComplete={USERNAME_AUTOCOMPLETE}
              disabled={adminCreated}
            />
          </Field>
          <Field>
            <FieldLabel>{t("welcome.fields.password")}</FieldLabel>
            <SecretInput
              value={password}
              onChange={setPassword}
              showStrength
              showGenerate
              autoComplete={NEW_PASSWORD_AUTOCOMPLETE}
            />
            <FieldDescription>{t("welcome.fields.passwordHint")}</FieldDescription>
          </Field>
          {!adminCreated ? (
            <Field>
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  checked={enableTotp}
                  onChange={(event) => setEnableTotp(event.target.checked)}
                />
                {t("welcome.fields.enableTotp")}
              </label>
            </Field>
          ) : null}
          {adminCreated && enableTotp && totpSecret && otpauthUri ? (
            <Card>
              <CardHeader>
                <CardTitle>{t("welcome.totp.title")}</CardTitle>
                <CardDescription>{t("welcome.totp.description")}</CardDescription>
              </CardHeader>
              <CardPanel className="flex flex-col gap-4">
                <CopyValue value={otpauthUri} label={t("welcome.totp.uriLabel")} />
                <CopyValue value={totpSecret} label={t("welcome.totp.secretLabel")} />
                <Field>
                  <FieldLabel>{t("welcome.totp.confirmLabel")}</FieldLabel>
                  <TotpInput value={totpConfirmCode} onChange={setTotpConfirmCode} />
                </Field>
              </CardPanel>
            </Card>
          ) : null}
        </div>
      ) : null}

      {step === 1 ? (
        <div className="flex flex-col gap-4">
          {doctorQuery.error ? <Banner tone="error" title={doctorQuery.error} /> : null}
          {doctorChecks === null && !doctorQuery.error ? (
            <p className="text-muted-foreground text-sm">{t("loading.label")}</p>
          ) : null}
          {doctorChecks ? (
            <>
              <StackedChecks checks={doctorChecks.filter((check) => !check.id.startsWith("host_"))} />
              <div className="flex flex-col gap-3">
                <h3 className="font-medium">{t("welcome.q76.heading")}</h3>
                <p className="text-muted-foreground text-sm">{t("welcome.q76.description")}</p>
                {q76Panels.map(({ id, labelKey, check }) => (
                  <Card key={id}>
                    <CardHeader>
                      <CardTitle>{t(labelKey)}</CardTitle>
                      <CardDescription>
                        {check?.message ?? t("welcome.q76.noneFound")}
                      </CardDescription>
                    </CardHeader>
                    {check ? (
                      <CardPanel className="flex flex-wrap gap-2">
                        <Button
                          type="button"
                          size="sm"
                          variant={q76Choices[id] === Q76_IMPORT ? "default" : "outline"}
                          onClick={() => setQ76Choices((current) => ({ ...current, [id]: Q76_IMPORT }))}
                        >
                          {t("welcome.q76.import")}
                        </Button>
                        <Button
                          type="button"
                          size="sm"
                          variant={q76Choices[id] === Q76_LEAVE ? "default" : "outline"}
                          onClick={() => setQ76Choices((current) => ({ ...current, [id]: Q76_LEAVE }))}
                        >
                          {t("welcome.q76.leaveUnmanaged")}
                        </Button>
                      </CardPanel>
                    ) : null}
                  </Card>
                ))}
              </div>
            </>
          ) : null}
        </div>
      ) : null}

      {step === 2 ? (
        <div className="flex flex-col gap-4">
          <Field>
            <FieldLabel>{t("welcome.fields.hostname")}</FieldLabel>
            <Input
              value={hostname}
              onChange={(event) => setHostname(event.target.value)}
              placeholder={t("welcome.fields.hostnamePlaceholder")}
            />
            <FieldDescription>{t("welcome.fields.hostnameHint")}</FieldDescription>
          </Field>
          <Field>
            <FieldLabel>{t("welcome.fields.timezone")}</FieldLabel>
            <TimezoneSelect value={timezone} onChange={setTimezone} />
          </Field>
          <Field>
            <FieldLabel>{t("welcome.fields.notificationChannel")}</FieldLabel>
            <Select
              value={channelType}
              onValueChange={(value) => value && setChannelType(value as NotificationChannelType)}
              disabled={skipNotification}
            >
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectPopup>
                {(Object.keys(CHANNEL_ICONS) as NotificationChannelType[]).map((type) => {
                  const Icon = CHANNEL_ICONS[type];
                  return (
                    <SelectItem key={type} value={type}>
                      <span className="flex items-center gap-2">
                        <Icon aria-hidden="true" className="size-4" />
                        {t(`welcome.channelTypes.${type}`)}
                      </span>
                    </SelectItem>
                  );
                })}
              </SelectPopup>
            </Select>
            {!skipNotification ? (
              <>
                <Input
                  value={channelName}
                  onChange={(event) => setChannelName(event.target.value)}
                  placeholder={t("welcome.fields.channelNamePlaceholder")}
                  className="mt-2"
                />
                <SecretInput
                  value={channelSecret}
                  onChange={setChannelSecret}
                  placeholder={t("welcome.fields.channelSecretPlaceholder")}
                />
              </>
            ) : null}
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="self-start"
              onClick={() => setSkipNotification((current) => !current)}
            >
              {skipNotification ? t("welcome.actions.configureChannel") : t("welcome.actions.skipChannel")}
            </Button>
            {skipNotification ? (
              <Banner tone="warning" title={t("welcome.warnings.skipNotificationTitle")} description={t("welcome.warnings.skipNotificationDescription")} />
            ) : null}
          </Field>
          <Field>
            <FieldLabel>{t("welcome.fields.backupPassphrase")}</FieldLabel>
            {!skipBackupPassphrase ? (
              <SecretInput
                value={backupPassphrase}
                onChange={setBackupPassphrase}
                showStrength
                showGenerate
              />
            ) : null}
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="self-start"
              onClick={() => setSkipBackupPassphrase((current) => !current)}
            >
              {skipBackupPassphrase
                ? t("welcome.actions.setBackupPassphrase")
                : t("welcome.actions.skipBackupPassphrase")}
            </Button>
            {skipBackupPassphrase ? (
              <Banner tone="warning" title={t("welcome.warnings.skipBackupTitle")} description={t("welcome.warnings.skipBackupDescription")} />
            ) : null}
          </Field>
        </div>
      ) : null}

      {step === 3 ? (
        <ChoiceCards
          name={ONBOARDING_PATH_NAME}
          value={pathChoice}
          onChange={setPathChoice}
          options={[
            {
              value: PATH_FRESH,
              title: t("welcome.path.fresh.title"),
              description: t("welcome.path.fresh.description"),
              icon: <HardDrive aria-hidden="true" className="size-5" />,
            },
            {
              value: PATH_MIGRATE,
              title: t("welcome.path.migrate.title"),
              description: t("welcome.path.migrate.description"),
              icon: <Server aria-hidden="true" className="size-5" />,
            },
          ]}
        />
      ) : null}
    </Wizard>
  );
}
