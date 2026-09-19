import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { InlineNote } from "@/components/patterns/inline-note";
import { SettingSwitch } from "@/components/patterns/setting-switch";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Field, FieldLabel } from "@/components/ui/field";
import { Frame, FramePanel } from "@/components/ui/frame";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  MAINTENANCE_CHAIN_STEPS,
  OTHER_SCHEDULE_JOBS,
  type MaintenanceChainStepId,
  type OtherScheduleJobId,
} from "@/lib/maintenance-chain";

const CHAIN_SCHEDULE_PREVIEW = "settings.schedules.chain.schedulePreview";
const OTHER_FREQUENCY_OPTIONS = ["daily", "weekly", "monthly"] as const;

function defaultChainEnabled(): Record<MaintenanceChainStepId, boolean> {
  return {
    mover: true,
    diff_guard: true,
    sync: true,
    scrub: true,
    config_backup: true,
  };
}

function defaultOtherJobs(): Record<
  OtherScheduleJobId,
  { enabled: boolean; frequency: typeof OTHER_FREQUENCY_OPTIONS[number]; time: string }
> {
  return {
    smart_self_test: { enabled: true, frequency: "weekly", time: "03:00" },
    appdata_backup: { enabled: false, frequency: "daily", time: "04:00" },
    restore_drill: { enabled: false, frequency: "monthly", time: "05:00" },
    container_update_check: { enabled: true, frequency: "daily", time: "06:00" },
  };
}

export function SchedulesSettingsPage(): React.ReactElement {
  const { t } = useTranslation();
  const [chainEnabled, setChainEnabled] = useState(defaultChainEnabled);
  const [otherJobs, setOtherJobs] = useState(defaultOtherJobs);
  const conflicts: string[] = [];

  return (
    <div className="flex flex-col gap-4">
      {conflicts.length > 0 ? (
        <Banner
          tone="warning"
          title={t("settings.schedules.conflictTitle")}
          description={conflicts.join(" ")}
        />
      ) : null}

      <InlineNote description={t("settings.schedules.apiNote")} />

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.schedules.chainTitle")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-4">
          <p className="text-muted-foreground text-sm">{t("settings.schedules.chainDescription")}</p>
          <div className="grid gap-2 sm:grid-cols-2">
            <div>
              <p className="font-medium text-sm">{t("settings.schedules.chainSchedule")}</p>
              <p className="text-muted-foreground text-sm">{t(CHAIN_SCHEDULE_PREVIEW)}</p>
            </div>
            <div>
              <p className="font-medium text-sm">{t("settings.schedules.nextRun")}</p>
              <p className="text-muted-foreground text-sm">{t("settings.schedules.nextRunUnavailable")}</p>
            </div>
          </div>
          <Frame>
            {MAINTENANCE_CHAIN_STEPS.map((stepId) => (
              <FramePanel key={stepId}>
                <SettingSwitch
                  label={t(`settings.schedules.chainSteps.${stepId}.title`)}
                  description={t(`settings.schedules.chainSteps.${stepId}.description`)}
                  checked={chainEnabled[stepId]}
                  onCheckedChange={(enabled) =>
                    setChainEnabled((current) => ({ ...current, [stepId]: enabled }))
                  }
                />
              </FramePanel>
            ))}
          </Frame>
        </CardPanel>
      </Card>

      <div className="flex flex-col gap-3">
        <h2 className="font-medium text-lg">{t("settings.schedules.otherJobsTitle")}</h2>
        <p className="text-muted-foreground text-sm">{t("settings.schedules.otherJobsDescription")}</p>
        {OTHER_SCHEDULE_JOBS.map((jobId) => {
          const job = otherJobs[jobId];
          return (
            <Card key={jobId}>
              <CardPanel className="flex flex-col gap-4">
                <SettingSwitch
                  label={t(`settings.schedules.otherJobs.${jobId}.title`)}
                  description={t(`settings.schedules.otherJobs.${jobId}.description`)}
                  checked={job.enabled}
                  onCheckedChange={(enabled) =>
                    setOtherJobs((current) => ({
                      ...current,
                      [jobId]: { ...current[jobId], enabled },
                    }))
                  }
                />
                <div className="grid gap-4 sm:grid-cols-3">
                  <Field>
                    <FieldLabel>{t("settings.schedules.frequency")}</FieldLabel>
                    <Select
                      value={job.frequency}
                      disabled={!job.enabled}
                      onValueChange={(value) =>
                        value &&
                        setOtherJobs((current) => ({
                          ...current,
                          [jobId]: {
                            ...current[jobId],
                            frequency: value as typeof job.frequency,
                          },
                        }))
                      }
                    >
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectPopup>
                        {OTHER_FREQUENCY_OPTIONS.map((frequency) => (
                          <SelectItem key={frequency} value={frequency}>
                            {t(`settings.schedules.frequencies.${frequency}`)}
                          </SelectItem>
                        ))}
                      </SelectPopup>
                    </Select>
                  </Field>
                  <Field>
                    <FieldLabel>{t("settings.schedules.time")}</FieldLabel>
                    <Input
                      type="time"
                      value={job.time}
                      disabled={!job.enabled}
                      onChange={(event) =>
                        setOtherJobs((current) => ({
                          ...current,
                          [jobId]: { ...current[jobId], time: event.target.value },
                        }))
                      }
                    />
                  </Field>
                  <Field>
                    <FieldLabel>{t("settings.schedules.nextRun")}</FieldLabel>
                    <p className="text-muted-foreground text-sm">{t("settings.schedules.nextRunUnavailable")}</p>
                  </Field>
                </div>
              </CardPanel>
            </Card>
          );
        })}
      </div>
    </div>
  );
}
