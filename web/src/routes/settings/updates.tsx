import { useState } from "react";
import { useTranslation } from "react-i18next";

import { ConfirmDialog } from "@/components/patterns/confirm";
import { showFeedbackToast } from "@/components/patterns/feedback-toast";
import { InlineNote } from "@/components/patterns/inline-note";
import { SegmentedChoice } from "@/components/patterns/segmented-choice";
import { SettingSwitch } from "@/components/patterns/setting-switch";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { ScrollArea } from "@/components/ui/scroll-area";

type ConfirmAction = "update" | "rollback" | "reboot" | null;

const UPDATE_CHANNEL_STABLE = "stable";
const UPDATE_CHANNEL_BETA = "beta";
const UPDATE_CHANNEL_FIELD = "update-channel";
const CONFIRM_UPDATE = "update";
const CONFIRM_ROLLBACK = "rollback";
const CONFIRM_REBOOT = "reboot";

export function UpdatesSettingsPage(): React.ReactElement {
  const { t } = useTranslation();
  const [channel, setChannel] = useState(UPDATE_CHANNEL_STABLE);
  const [updateCheckEnabled, setUpdateCheckEnabled] = useState(true);
  const [confirmAction, setConfirmAction] = useState<ConfirmAction>(null);

  function handleConfirmedAction(): void {
    setConfirmAction(null);
    showFeedbackToast({
      type: "warning",
      title: t("settings.updates.actionUnavailable"),
      description: t("settings.updates.actionUnavailableDescription"),
    });
  }

  return (
    <div className="flex flex-col gap-4">
      <InlineNote description={t("settings.updates.apiNote")} />

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.updates.versionTitle")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-4">
          <div className="flex flex-wrap items-center gap-3">
            <div>
              <p className="font-medium text-sm">{t("settings.updates.currentVersion")}</p>
              <p className="text-muted-foreground text-sm">{t("settings.updates.versionUnknown")}</p>
            </div>
            <StatusBadge tone="outline">{t("settings.updates.noUpdate")}</StatusBadge>
          </div>
          <div>
            <p className="mb-2 font-medium text-sm">{t("settings.updates.changelog")}</p>
            <ScrollArea className="h-32 rounded-lg border p-3">
              <p className="text-muted-foreground text-sm">{t("settings.updates.changelogEmpty")}</p>
            </ScrollArea>
          </div>
          <div className="flex flex-col gap-2">
            <p className="font-medium text-sm">{t("settings.updates.channel")}</p>
            <SegmentedChoice
              name={UPDATE_CHANNEL_FIELD}
              value={channel}
              onChange={setChannel}
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
            onCheckedChange={setUpdateCheckEnabled}
          />
        </CardPanel>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.updates.dependenciesTitle")}</CardTitle>
        </CardHeader>
        <CardPanel>
          <p className="text-muted-foreground text-sm">{t("settings.updates.dependenciesEmpty")}</p>
        </CardPanel>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.updates.systemTitle")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-4">
          <p className="text-muted-foreground text-sm">{t("settings.updates.systemDescription")}</p>
          <div className="flex flex-wrap gap-2">
            <Button type="button" onClick={() => setConfirmAction(CONFIRM_UPDATE)}>
              {t("settings.updates.actions.update")}
            </Button>
            <Button type="button" variant="outline" onClick={() => setConfirmAction(CONFIRM_ROLLBACK)}>
              {t("settings.updates.actions.rollback")}
            </Button>
            <Button type="button" variant="outline" onClick={() => setConfirmAction(CONFIRM_REBOOT)}>
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
        onConfirm={handleConfirmedAction}
      />
      <ConfirmDialog
        open={confirmAction === CONFIRM_ROLLBACK}
        onOpenChange={(open) => !open && setConfirmAction(null)}
        title={t("settings.updates.confirm.rollback.title")}
        description={t("settings.updates.confirm.rollback.description")}
        confirmLabel={t("settings.updates.actions.rollback")}
        destructive
        onConfirm={handleConfirmedAction}
      />
      <ConfirmDialog
        open={confirmAction === CONFIRM_REBOOT}
        onOpenChange={(open) => !open && setConfirmAction(null)}
        title={t("settings.updates.confirm.reboot.title")}
        description={t("settings.updates.confirm.reboot.description")}
        confirmLabel={t("settings.updates.actions.reboot")}
        onConfirm={handleConfirmedAction}
      />
    </div>
  );
}
