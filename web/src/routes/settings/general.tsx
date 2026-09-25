import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { ChoiceCards } from "@/components/patterns/choice-cards";
import { LoadingBlock } from "@/components/patterns/loading";
import { NumberUnit } from "@/components/patterns/number-unit";
import { SecretInput } from "@/components/patterns/secret-input";
import { TimezoneSelect } from "@/components/patterns/timezone";
import { Button } from "@/components/ui/button";
import { Card, CardFooter, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import type { components } from "@/lib/api/client";
import { getGeneralSettings, getUPSSettings, putGeneralSettings, putUPSSettings } from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";

type GeneralSettings = components["schemas"]["GeneralSettings"];
type UPSSettings = components["schemas"]["UPSSettings"];
type UPSConnection = components["schemas"]["UPSConnection"];

const UPS_CONNECTION_FIELD = "ups-connection";
const UPS_DRIVER_ID = "ups-driver";
const UPS_PORT_ID = "ups-port";
const UPS_MONITOR_PASSWORD_ID = "ups-monitor-password";
const UPS_LOW_BATTERY_ID = "ups-low-battery";
const UPS_RUNTIME_ID = "ups-runtime";
const UPS_NETWORK_HOST_ID = "ups-network-host";
const UPS_NETWORK_PORT_ID = "ups-network-port";
const UPS_NETWORK_NAME_ID = "ups-network-name";
const UPS_NETWORK_USER_ID = "ups-network-user";
const UPS_NETWORK_PASSWORD_ID = "ups-network-password";

type UPSFormState = {
  connection: UPSConnection;
  driver: string;
  port: string;
  monitorPassword: string;
  networkHost: string;
  networkPort: number;
  networkUpsName: string;
  networkUsername: string;
  networkPassword: string;
  lowBatteryPercent: number;
  runtimeSeconds: number;
};

const defaultUPSForm = (): UPSFormState => ({
  connection: "usb",
  driver: "usbhid-ups",
  port: "auto",
  monitorPassword: "",
  networkHost: "",
  networkPort: 3493,
  networkUpsName: "",
  networkUsername: "",
  networkPassword: "",
  lowBatteryPercent: 20,
  runtimeSeconds: 300,
});

function upsFormFromSettings(data: UPSSettings): UPSFormState {
  const base = defaultUPSForm();
  if (!data.configured) {
    return base;
  }
  return {
    connection: data.connection ?? "usb",
    driver: data.driver ?? base.driver,
    port: data.port ?? base.port,
    monitorPassword: "",
    networkHost: data.networkHost ?? "",
    networkPort: data.networkPort ?? 3493,
    networkUpsName: data.networkUpsName ?? "",
    networkUsername: data.networkUsername ?? "",
    networkPassword: "",
    lowBatteryPercent: data.lowBatteryPercent ?? 0,
    runtimeSeconds: data.runtimeSeconds ?? 0,
  };
}

function upsDirty(form: UPSFormState, saved: UPSSettings | null, passwordTouched: {
  monitor: boolean;
  network: boolean;
}): boolean {
  if (saved === null) {
    return false;
  }
  if (!saved.configured) {
    return (
      form.connection !== "usb" ||
      form.driver !== "usbhid-ups" ||
      form.port !== "auto" ||
      form.monitorPassword !== "" ||
      form.networkHost !== "" ||
      form.networkPort !== 3493 ||
      form.networkUpsName !== "" ||
      form.networkUsername !== "" ||
      form.networkPassword !== "" ||
      form.lowBatteryPercent !== 20 ||
      form.runtimeSeconds !== 300 ||
      passwordTouched.monitor ||
      passwordTouched.network
    );
  }
  const savedForm = upsFormFromSettings(saved);
  return (
    form.connection !== savedForm.connection ||
    form.driver !== savedForm.driver ||
    form.port !== savedForm.port ||
    form.networkHost !== savedForm.networkHost ||
    form.networkPort !== savedForm.networkPort ||
    form.networkUpsName !== savedForm.networkUpsName ||
    form.networkUsername !== savedForm.networkUsername ||
    form.lowBatteryPercent !== savedForm.lowBatteryPercent ||
    form.runtimeSeconds !== savedForm.runtimeSeconds ||
    passwordTouched.monitor ||
    passwordTouched.network
  );
}

export function GeneralSettingsPage(): React.ReactElement {
  const { t } = useTranslation();
  const generalQuery = useApiQuery<GeneralSettings>({
    queryKey: "general-settings",
    queryFn: (signal) => getGeneralSettings(signal),
    fallbackError: t("settings.general.loadFailed"),
  });
  const upsQuery = useApiQuery<UPSSettings>({
    queryKey: "ups-settings",
    queryFn: (signal) => getUPSSettings(signal),
    fallbackError: t("settings.ups.loadFailed"),
  });
  const generalMutation = useApiMutation<{ hostname: string; timezone: string }, GeneralSettings>({
    mutationFn: (body) => putGeneralSettings(body),
    fallbackError: t("settings.general.saveFailed"),
  });
  const upsMutation = useApiMutation<components["schemas"]["UpdateUPSSettingsRequest"], UPSSettings>({
    mutationFn: (body) => putUPSSettings(body),
    fallbackError: t("settings.ups.saveFailed"),
  });

  const saved = generalQuery.data;
  const upsSaved = upsQuery.data;
  const [hostname, setHostname] = useState("");
  const [timezone, setTimezone] = useState(() => Intl.DateTimeFormat().resolvedOptions().timeZone);
  const [upsForm, setUpsForm] = useState<UPSFormState>(defaultUPSForm);
  const [passwordTouched, setPasswordTouched] = useState({ monitor: false, network: false });
  // Set at handler entry and cleared in `finally`, so Save/Cancel stay
  // disabled across the whole handler, not just while the mutation's own
  // `pending` flag is true — that flag drops back to false while the
  // trailing `refresh()` is still in flight and `dirty`/`upsIsDirty` are
  // still true (the query's `data` hasn't caught up yet), which would let
  // a second click re-submit the same save (issue #271 finding).
  const [generalSaveBusy, setGeneralSaveBusy] = useState(false);
  const [upsSaveBusy, setUpsSaveBusy] = useState(false);

  // Adjusts state when the loaded (or just-saved) settings object changes,
  // rather than in an effect (react-hooks/set-state-in-effect) — the draft
  // resets to match the server exactly once per new `saved`/`upsSaved`
  // reference.
  const [seededGeneral, setSeededGeneral] = useState<GeneralSettings | null>(null);
  if (saved && saved !== seededGeneral) {
    setSeededGeneral(saved);
    setHostname(saved.hostname ?? "");
    setTimezone(saved.timezone ?? Intl.DateTimeFormat().resolvedOptions().timeZone);
  }

  const [seededUps, setSeededUps] = useState<UPSSettings | null>(null);
  if (upsSaved && upsSaved !== seededUps) {
    setSeededUps(upsSaved);
    setUpsForm(upsFormFromSettings(upsSaved));
    setPasswordTouched({ monitor: false, network: false });
  }

  const dirty =
    saved !== null &&
    (hostname !== (saved.hostname ?? "") ||
      timezone !== (saved.timezone ?? Intl.DateTimeFormat().resolvedOptions().timeZone));
  const upsIsDirty = upsDirty(upsForm, upsSaved, passwordTouched);

  async function handleSave(): Promise<void> {
    setGeneralSaveBusy(true);
    try {
      const result = await generalMutation.mutate({ hostname, timezone });
      if (!result.ok) {
        return;
      }
      await generalQuery.refresh();
    } finally {
      setGeneralSaveBusy(false);
    }
  }

  function handleCancel(): void {
    generalMutation.reset();
    if (!saved) {
      return;
    }
    setHostname(saved.hostname ?? "");
    setTimezone(saved.timezone ?? Intl.DateTimeFormat().resolvedOptions().timeZone);
  }

  async function handleUPSSave(): Promise<void> {
    const body: components["schemas"]["UpdateUPSSettingsRequest"] = {
      connection: upsForm.connection,
    };
    if (upsForm.connection === "usb") {
      body.driver = upsForm.driver;
      body.port = upsForm.port;
      if (upsForm.monitorPassword !== "") {
        body.monitorPassword = upsForm.monitorPassword;
      }
      body.lowBatteryPercent = upsForm.lowBatteryPercent;
      body.runtimeSeconds = upsForm.runtimeSeconds;
    } else {
      body.networkHost = upsForm.networkHost;
      body.networkPort = upsForm.networkPort;
      body.networkUpsName = upsForm.networkUpsName;
      body.networkUsername = upsForm.networkUsername;
      if (upsForm.networkPassword !== "") {
        body.networkPassword = upsForm.networkPassword;
      }
    }
    setUpsSaveBusy(true);
    try {
      const result = await upsMutation.mutate(body);
      if (!result.ok) {
        return;
      }
      await upsQuery.refresh();
    } finally {
      setUpsSaveBusy(false);
    }
  }

  function handleUPSCancel(): void {
    upsMutation.reset();
    if (!upsSaved) {
      return;
    }
    setUpsForm(upsFormFromSettings(upsSaved));
    setPasswordTouched({ monitor: false, network: false });
  }

  if (generalQuery.loading || upsQuery.loading) {
    return <LoadingBlock />;
  }

  const connectionOptions = [
    {
      value: "usb",
      title: t("settings.ups.connectionUsb"),
      description: t("settings.ups.connectionUsbDescription"),
    },
    {
      value: "network",
      title: t("settings.ups.connectionNetwork"),
      description: t("settings.ups.connectionNetworkDescription"),
    },
  ];

  const error = generalQuery.error ?? generalMutation.error;
  const upsError = upsQuery.error ?? upsMutation.error;
  const saving = generalSaveBusy || generalMutation.pending;
  const upsSaving = upsSaveBusy || upsMutation.pending;

  return (
    <div className="flex flex-col gap-4">
      {error ? <Banner tone="error" title={error} /> : null}

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.general.title")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-4">
          <Field>
            <FieldLabel>{t("settings.general.hostname")}</FieldLabel>
            <Input
              value={hostname}
              onChange={(event) => setHostname(event.target.value)}
              placeholder={t("settings.general.hostnamePlaceholder")}
            />
            <FieldDescription>{t("settings.general.hostnameHint")}</FieldDescription>
          </Field>
          <Field>
            <FieldLabel>{t("settings.general.timezone")}</FieldLabel>
            <TimezoneSelect value={timezone} onChange={setTimezone} />
          </Field>
        </CardPanel>
        <CardFooter className="flex justify-end gap-2 border-t">
          <Button type="button" variant="outline" disabled={!dirty || saving} onClick={handleCancel}>
            {t("settings.actions.cancel")}
          </Button>
          <Button type="button" loading={saving} disabled={!dirty} onClick={() => void handleSave()}>
            {t("settings.actions.save")}
          </Button>
        </CardFooter>
      </Card>

      {upsError ? <Banner tone="error" title={upsError} /> : null}

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.ups.title")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-4">
          {upsSaved !== null && !upsSaved.configured ? (
            <p className="text-muted-foreground text-sm">{t("settings.ups.emptyDescription")}</p>
          ) : null}

          <Field>
            <FieldLabel>{t("settings.ups.connection")}</FieldLabel>
            <ChoiceCards
              name={UPS_CONNECTION_FIELD}
              value={upsForm.connection}
              onChange={(value) =>
                setUpsForm((current) => ({ ...current, connection: value as UPSConnection }))
              }
              options={connectionOptions}
            />
            <FieldDescription>{t("settings.ups.connectionHint")}</FieldDescription>
          </Field>

          {upsForm.connection === "usb" ? (
            <>
              <Field>
                <FieldLabel htmlFor={UPS_DRIVER_ID}>{t("settings.ups.driver")}</FieldLabel>
                <Input
                  id={UPS_DRIVER_ID}
                  value={upsForm.driver}
                  onChange={(event) => setUpsForm((current) => ({ ...current, driver: event.target.value }))}
                  placeholder={t("settings.ups.driverPlaceholder")}
                />
                <FieldDescription>{t("settings.ups.driverHint")}</FieldDescription>
              </Field>
              <Field>
                <FieldLabel htmlFor={UPS_PORT_ID}>{t("settings.ups.port")}</FieldLabel>
                <Input
                  id={UPS_PORT_ID}
                  value={upsForm.port}
                  onChange={(event) => setUpsForm((current) => ({ ...current, port: event.target.value }))}
                  placeholder={t("settings.ups.portPlaceholder")}
                />
              </Field>
              <Field>
                <FieldLabel htmlFor={UPS_MONITOR_PASSWORD_ID}>{t("settings.ups.monitorPassword")}</FieldLabel>
                <SecretInput
                  id={UPS_MONITOR_PASSWORD_ID}
                  value={upsForm.monitorPassword}
                  onChange={(value) => {
                    setPasswordTouched((current) => ({ ...current, monitor: true }));
                    setUpsForm((current) => ({ ...current, monitorPassword: value }));
                  }}
                  placeholder={
                    upsSaved?.monitorPasswordSet
                      ? t("settings.ups.passwordKeepPlaceholder")
                      : t("settings.ups.passwordPlaceholder")
                  }
                />
                <FieldDescription>{t("settings.ups.monitorPasswordHint")}</FieldDescription>
              </Field>
              <Field>
                <FieldLabel htmlFor={UPS_LOW_BATTERY_ID}>{t("settings.ups.lowBatteryPercent")}</FieldLabel>
                <NumberUnit
                  id={UPS_LOW_BATTERY_ID}
                  value={upsForm.lowBatteryPercent}
                  onChange={(value) => setUpsForm((current) => ({ ...current, lowBatteryPercent: value }))}
                  unit={t("settings.ups.percentUnit")}
                  min={0}
                  max={100}
                />
                <FieldDescription>{t("settings.ups.lowBatteryPercentHint")}</FieldDescription>
              </Field>
              <Field>
                <FieldLabel htmlFor={UPS_RUNTIME_ID}>{t("settings.ups.runtimeSeconds")}</FieldLabel>
                <NumberUnit
                  id={UPS_RUNTIME_ID}
                  value={upsForm.runtimeSeconds}
                  onChange={(value) => setUpsForm((current) => ({ ...current, runtimeSeconds: value }))}
                  unit={t("settings.ups.secondsUnit")}
                  min={0}
                />
                <FieldDescription>{t("settings.ups.runtimeSecondsHint")}</FieldDescription>
              </Field>
            </>
          ) : (
            <>
              <Field>
                <FieldLabel htmlFor={UPS_NETWORK_HOST_ID}>{t("settings.ups.networkHost")}</FieldLabel>
                <Input
                  id={UPS_NETWORK_HOST_ID}
                  value={upsForm.networkHost}
                  onChange={(event) =>
                    setUpsForm((current) => ({ ...current, networkHost: event.target.value }))
                  }
                  placeholder={t("settings.ups.networkHostPlaceholder")}
                />
              </Field>
              <Field>
                <FieldLabel htmlFor={UPS_NETWORK_PORT_ID}>{t("settings.ups.networkPort")}</FieldLabel>
                <NumberUnit
                  id={UPS_NETWORK_PORT_ID}
                  value={upsForm.networkPort}
                  onChange={(value) => setUpsForm((current) => ({ ...current, networkPort: value }))}
                  unit={t("settings.ups.portUnit")}
                  min={1}
                  max={65535}
                />
              </Field>
              <Field>
                <FieldLabel htmlFor={UPS_NETWORK_NAME_ID}>{t("settings.ups.networkUpsName")}</FieldLabel>
                <Input
                  id={UPS_NETWORK_NAME_ID}
                  value={upsForm.networkUpsName}
                  onChange={(event) =>
                    setUpsForm((current) => ({ ...current, networkUpsName: event.target.value }))
                  }
                  placeholder={t("settings.ups.networkUpsNamePlaceholder")}
                />
                <FieldDescription>{t("settings.ups.networkUpsNameHint")}</FieldDescription>
              </Field>
              <Field>
                <FieldLabel htmlFor={UPS_NETWORK_USER_ID}>{t("settings.ups.networkUsername")}</FieldLabel>
                <Input
                  id={UPS_NETWORK_USER_ID}
                  value={upsForm.networkUsername}
                  onChange={(event) =>
                    setUpsForm((current) => ({ ...current, networkUsername: event.target.value }))
                  }
                />
              </Field>
              <Field>
                <FieldLabel htmlFor={UPS_NETWORK_PASSWORD_ID}>{t("settings.ups.networkPassword")}</FieldLabel>
                <SecretInput
                  id={UPS_NETWORK_PASSWORD_ID}
                  value={upsForm.networkPassword}
                  onChange={(value) => {
                    setPasswordTouched((current) => ({ ...current, network: true }));
                    setUpsForm((current) => ({ ...current, networkPassword: value }));
                  }}
                  placeholder={
                    upsSaved?.networkPasswordSet
                      ? t("settings.ups.passwordKeepPlaceholder")
                      : t("settings.ups.passwordPlaceholder")
                  }
                />
              </Field>
            </>
          )}
        </CardPanel>
        <CardFooter className="flex justify-end gap-2 border-t">
          <Button
            type="button"
            variant="outline"
            disabled={!upsIsDirty || upsSaving}
            onClick={handleUPSCancel}
          >
            {t("settings.actions.cancel")}
          </Button>
          <Button
            type="button"
            loading={upsSaving}
            disabled={!upsIsDirty}
            onClick={() => void handleUPSSave()}
          >
            {t("settings.actions.save")}
          </Button>
        </CardFooter>
      </Card>
    </div>
  );
}
