import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { ConfirmDialog } from "@/components/patterns/confirm";
import { showFeedbackToast } from "@/components/patterns/feedback-toast";
import { InlineNote } from "@/components/patterns/inline-note";
import { LoadingBlock } from "@/components/patterns/loading";
import { SegmentedChoice } from "@/components/patterns/segmented-choice";
import { SettingSwitch } from "@/components/patterns/setting-switch";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { ScrollArea } from "@/components/ui/scroll-area";
import { hoservaClient, type components } from "@/lib/api/client";

type ConfirmAction = "update" | "rollback" | "reboot" | null;
type UpdateStatus = components["schemas"]["UpdateStatus"];
type UpdateChannel = NonNullable<UpdateStatus["channel"]>;

const UPDATE_CHANNEL_STABLE = "stable";
const UPDATE_CHANNEL_BETA = "beta";
const UPDATE_CHANNEL_FIELD = "update-channel";
const CONFIRM_UPDATE = "update";
const CONFIRM_ROLLBACK = "rollback";
const CONFIRM_REBOOT = "reboot";

const CONFIRM_PATH: Record<Exclude<ConfirmAction, null>, "/settings/updates/apply" | "/settings/updates/rollback" | "/settings/updates/reboot"> = {
  update: "/settings/updates/apply",
  rollback: "/settings/updates/rollback",
  reboot: "/settings/updates/reboot",
};

export function UpdatesSettingsPage(): React.ReactElement {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [status, setStatus] = useState<UpdateStatus | null>(null);
  const [channel, setChannel] = useState<UpdateChannel>(UPDATE_CHANNEL_STABLE);
  const [updateCheckEnabled, setUpdateCheckEnabled] = useState(true);
  const [confirmAction, setConfirmAction] = useState<ConfirmAction>(null);
  const [actionLoading, setActionLoading] = useState(false);

  function applyStatus(next: UpdateStatus): void {
    setStatus(next);
    setChannel(next.channel);
    setUpdateCheckEnabled(next.checkEnabled);
  }

  useEffect(() => {
    const controller = new AbortController();
    hoservaClient
      .GET("/settings/updates", { signal: controller.signal })
      .then(({ data, error: apiError }) => {
        if (controller.signal.aborted) {
          return;
        }
        if (apiError) {
          setError(apiError.message);
          return;
        }
        if (data) {
          applyStatus(data);
        }
      })
      .catch((err: unknown) => {
        if (!controller.signal.aborted) {
          setError(err instanceof Error ? err.message : String(err));
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

  async function persistSettings(body: { channel?: UpdateChannel; checkEnabled?: boolean }): Promise<void> {
    setError(null);
    const { data, error: apiError } = await hoservaClient.PUT("/settings/updates", { body });
    if (apiError) {
      setError(apiError.message);
      return;
    }
    if (data) {
      applyStatus(data);
    }
  }

  async function handleConfirmedAction(): Promise<void> {
    const action = confirmAction;
    setConfirmAction(null);
    if (!action) {
      return;
    }
    setActionLoading(true);
    setError(null);
    try {
      const { data, error: apiError } = await hoservaClient.POST(CONFIRM_PATH[action], {
        body: { confirm: true },
      });
      if (apiError) {
        setError(apiError.message);
        showFeedbackToast({
          type: "error",
          title: t("settings.updates.actionFailed"),
          description: apiError.message,
        });
        return;
      }
      if (data) {
        applyStatus(data);
      }
      showFeedbackToast({
        type: "success",
        title: t(`settings.updates.queued.${action}`),
      });
    } finally {
      setActionLoading(false);
    }
  }

  if (loading) {
    return <LoadingBlock />;
  }

  const available = status?.availableVersion;
  const previous = status?.previousVersion;
  const rebootRequired = Boolean(status?.rebootRequired);
  const pending = status?.pendingDebianUpdates ?? [];
  const dependencies = status?.dependencies ?? [];

  return (
    <div className="flex flex-col gap-4">
      {error ? <Banner tone="error" title={error} /> : null}
      <InlineNote description={t("settings.updates.apiNote")} />

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.updates.versionTitle")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-4">
          <div className="flex flex-wrap items-center gap-3">
            <div>
              <p className="font-medium text-sm">{t("settings.updates.currentVersion")}</p>
              <p className="text-muted-foreground text-sm">{status?.currentVersion ?? t("settings.updates.versionUnknown")}</p>
            </div>
            {available ? (
              <StatusBadge tone="info">
                {t("settings.updates.available", { version: available })}
              </StatusBadge>
            ) : (
              <StatusBadge tone="outline">{t("settings.updates.noUpdate")}</StatusBadge>
            )}
            {rebootRequired ? <StatusBadge tone="warning">{t("settings.updates.rebootRequired")}</StatusBadge> : null}
          </div>
          <div>
            <p className="mb-2 font-medium text-sm">{t("settings.updates.changelog")}</p>
            <ScrollArea className="h-32 rounded-lg border p-3">
              <p className="text-muted-foreground text-sm">
                {status?.changelog ?? t("settings.updates.changelogEmpty")}
              </p>
            </ScrollArea>
          </div>
          <div>
            <p className="mb-2 font-medium text-sm">{t("settings.updates.channel")}</p>
            <SegmentedChoice
              name={UPDATE_CHANNEL_FIELD}
              value={channel}
              onChange={(value) => {
                const next = value === UPDATE_CHANNEL_BETA ? UPDATE_CHANNEL_BETA : UPDATE_CHANNEL_STABLE;
                setChannel(next);
                void persistSettings({ channel: next });
              }}
              options={[
                { value: UPDATE_CHANNEL_STABLE, label: t("settings.updates.channels.stable") },
                { value: UPDATE_CHANNEL_BETA, label: t("settings.updates.channels.beta") },
              ]}
            />
          </div>
          <SettingSwitch
            label={t("settings.updates.updateCheck")}
            description={t("settings.updates.updateCheckDescription")}
            checked={updateCheckEnabled}
            onCheckedChange={(checked) => {
              setUpdateCheckEnabled(checked);
              void persistSettings({ checkEnabled: checked });
            }}
          />
        </CardPanel>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.updates.dependenciesTitle")}</CardTitle>
        </CardHeader>
        <CardPanel>
          {dependencies.length === 0 ? (
            <p className="text-muted-foreground text-sm">{t("settings.updates.dependenciesEmpty")}</p>
          ) : (
            <ul className="flex flex-col gap-2 text-sm">
              {dependencies.map((dep) => (
                <li key={dep.name} className="flex flex-wrap items-center gap-2">
                  <span className="font-medium">{dep.name}</span>
                  <span className="text-muted-foreground">{dep.installedVersion || t("settings.updates.versionUnknown")}</span>
                  <StatusBadge tone={dep.inRange ? "success" : "warning"}>
                    {dep.inRange
                      ? t("settings.updates.dependencyInRange", { floor: dep.testedFloor })
                      : t("settings.updates.dependencyOutOfRange", { floor: dep.testedFloor })}
                  </StatusBadge>
                </li>
              ))}
            </ul>
          )}
        </CardPanel>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.updates.systemTitle")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-4">
          <p className="text-muted-foreground text-sm">{t("settings.updates.systemDescription")}</p>
          {pending.length === 0 ? (
            <p className="text-muted-foreground text-sm">{t("settings.updates.pendingDebianEmpty")}</p>
          ) : (
            <ul className="flex flex-col gap-1 text-sm">
              {pending.map((pkg) => (
                <li key={pkg.name}>
                  {pkg.name} {pkg.installedVersion} → {pkg.candidateVersion}
                </li>
              ))}
            </ul>
          )}
          <div className="flex flex-wrap gap-2">
            <Button type="button" disabled={!available || actionLoading} onClick={() => setConfirmAction(CONFIRM_UPDATE)}>
              {t("settings.updates.actions.update")}
            </Button>
            <Button
              type="button"
              variant="outline"
              disabled={!previous || actionLoading}
              onClick={() => setConfirmAction(CONFIRM_ROLLBACK)}
            >
              {t("settings.updates.actions.rollback")}
            </Button>
            <Button type="button" variant="outline" disabled={actionLoading} onClick={() => setConfirmAction(CONFIRM_REBOOT)}>
              {t("settings.updates.actions.reboot")}
            </Button>
          </div>
        </CardPanel>
      </Card>

      <ConfirmDialog
        open={confirmAction === CONFIRM_UPDATE}
        onOpenChange={(open) => !open && setConfirmAction(null)}
        title={t("settings.updates.confirm.update.title")}
        description={t("settings.updates.confirm.update.description")}
        confirmLabel={t("settings.updates.actions.update")}
        onConfirm={() => {
          void handleConfirmedAction();
        }}
      />
      <ConfirmDialog
        open={confirmAction === CONFIRM_ROLLBACK}
        onOpenChange={(open) => !open && setConfirmAction(null)}
        title={t("settings.updates.confirm.rollback.title")}
        description={t("settings.updates.confirm.rollback.description")}
        confirmLabel={t("settings.updates.actions.rollback")}
        destructive
        onConfirm={() => {
          void handleConfirmedAction();
        }}
      />
      <ConfirmDialog
        open={confirmAction === CONFIRM_REBOOT}
        onOpenChange={(open) => !open && setConfirmAction(null)}
        title={t("settings.updates.confirm.reboot.title")}
        description={t("settings.updates.confirm.reboot.description")}
        confirmLabel={t("settings.updates.actions.reboot")}
        onConfirm={() => {
          void handleConfirmedAction();
        }}
      />
    </div>
  );
}
