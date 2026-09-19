import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { LoadingBlock } from "@/components/patterns/loading";
import { TimezoneSelect } from "@/components/patterns/timezone";
import { Button } from "@/components/ui/button";
import { Card, CardFooter, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { hoservaClient, type components } from "@/lib/api/client";

type GeneralSettings = components["schemas"]["GeneralSettings"];

export function GeneralSettingsPage(): React.ReactElement {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState<GeneralSettings | null>(null);
  const [hostname, setHostname] = useState("");
  const [timezone, setTimezone] = useState(() => Intl.DateTimeFormat().resolvedOptions().timeZone);

  useEffect(() => {
    const controller = new AbortController();
    hoservaClient
      .GET("/settings/general", { signal: controller.signal })
      .then(({ data, error: apiError }) => {
        if (controller.signal.aborted) {
          return;
        }
        if (apiError) {
          setError(apiError.message);
          return;
        }
        if (data) {
          setSaved(data);
          setHostname(data.hostname ?? "");
          setTimezone(data.timezone ?? Intl.DateTimeFormat().resolvedOptions().timeZone);
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) {
          setLoading(false);
        }
      });
    return () => {
      controller.abort();
    };
  }, []);

  const dirty =
    saved !== null &&
    (hostname !== (saved.hostname ?? "") ||
      timezone !== (saved.timezone ?? Intl.DateTimeFormat().resolvedOptions().timeZone));

  async function handleSave(): Promise<void> {
    setError(null);
    setSaving(true);
    try {
      const { data, error: apiError } = await hoservaClient.PUT("/settings/general", {
        body: {
          hostname,
          timezone,
        },
      });
      if (apiError) {
        setError(apiError.message);
        return;
      }
      if (data) {
        setSaved(data);
        setHostname(data.hostname ?? "");
        setTimezone(data.timezone ?? timezone);
      }
    } finally {
      setSaving(false);
    }
  }

  function handleCancel(): void {
    if (!saved) {
      return;
    }
    setHostname(saved.hostname ?? "");
    setTimezone(saved.timezone ?? Intl.DateTimeFormat().resolvedOptions().timeZone);
    setError(null);
  }

  if (loading) {
    return <LoadingBlock />;
  }

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
    </div>
  );
}
