import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { LoadingBlock } from "@/components/patterns/loading";
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
import { hoservaClient, type components } from "@/lib/api/client";
import {
  MAINTENANCE_CHAIN_STEPS,
  OTHER_SCHEDULE_JOBS,
  type MaintenanceChainStepId,
  type OtherScheduleJobId,
} from "@/lib/maintenance-chain";

type Schedules = components["schemas"]["Schedules"];
type ScheduleFrequency = components["schemas"]["ScheduleFrequency"];

const OTHER_FREQUENCY_OPTIONS: ScheduleFrequency[] = ["daily", "weekly", "monthly"];

function formatNextRun(iso: string, locale: string): string {
  return new Intl.DateTimeFormat(locale, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(iso));
}

function conflictDescription(
  conflict: components["schemas"]["ScheduleConflict"],
  t: (key: string, options?: Record<string, string>) => string,
): string {
  return t("settings.schedules.conflictPair", {
    jobA: t(`settings.schedules.conflictJobs.${conflict.jobA}`, { defaultValue: conflict.jobA }),
    jobB: t(`settings.schedules.conflictJobs.${conflict.jobB}`, { defaultValue: conflict.jobB }),
  });
}

export function SchedulesSettingsPage(): React.ReactElement {
  const { t, i18n } = useTranslation();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [schedules, setSchedules] = useState<Schedules | null>(null);
  const [timeDrafts, setTimeDrafts] = useState<Partial<Record<OtherScheduleJobId, string>>>({});

  useEffect(() => {
    const controller = new AbortController();
    hoservaClient
      .GET("/settings/schedules", { signal: controller.signal })
      .then(({ data, error: apiError }) => {
        if (controller.signal.aborted) {
          return;
        }
        if (apiError) {
          setError(apiError.message);
          return;
        }
        if (data) {
          setSchedules(data);
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

  async function updateChainStep(stepId: MaintenanceChainStepId, enabled: boolean): Promise<void> {
    if (!schedules) {
      return;
    }
    setError(null);
    const steps = schedules.chain.steps.map((step) =>
      step.id === stepId ? { ...step, enabled } : step,
    );
    const { data, error: apiError } = await hoservaClient.PUT("/settings/schedules/chain", {
      body: { steps },
    });
    if (apiError) {
      setError(apiError.message);
      return;
    }
    if (data) {
      setSchedules(data);
    }
  }

  async function updateOtherJob(
    jobId: OtherScheduleJobId,
    patch: { enabled?: boolean; frequency?: ScheduleFrequency; time?: string },
  ): Promise<void> {
    if (!schedules) {
      return;
    }
    setError(null);
    const { data, error: apiError } = await hoservaClient.PUT("/settings/schedules/jobs/{jobId}", {
      params: { path: { jobId } },
      body: patch,
    });
    if (apiError) {
      setError(apiError.message);
      return;
    }
    if (data) {
      setSchedules(data);
    }
  }

  async function commitOtherJobTime(jobId: OtherScheduleJobId, persistedTime: string): Promise<void> {
    const draft = timeDrafts[jobId];
    if (draft === undefined) {
      return;
    }
    if (draft === "" || draft === persistedTime) {
      setTimeDrafts((current) => {
        const next = { ...current };
        delete next[jobId];
        return next;
      });
      return;
    }
    await updateOtherJob(jobId, { time: draft });
    setTimeDrafts((current) => {
      if (current[jobId] !== draft) {
        return current;
      }
      const next = { ...current };
      delete next[jobId];
      return next;
    });
  }

  if (loading) {
    return <LoadingBlock />;
  }

  if (!schedules) {
    return (
      <Banner
        tone="error"
        title={t("settings.schedules.loadErrorTitle")}
        description={error ?? t("settings.schedules.loadErrorDescription")}
      />
    );
  }

  const conflicts = schedules.conflicts.map((conflict) => conflictDescription(conflict, t));

  return (
    <div className="flex flex-col gap-4">
      {error ? (
        <Banner tone="error" title={t("settings.schedules.saveErrorTitle")} description={error} />
      ) : null}

      {conflicts.length > 0 ? (
        <Banner
          tone="warning"
          title={t("settings.schedules.conflictTitle")}
          description={conflicts.join(" ")}
        />
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.schedules.chainTitle")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-4">
          <p className="text-muted-foreground text-sm">{t("settings.schedules.chainDescription")}</p>
          <div className="grid gap-2 sm:grid-cols-2">
            <div>
              <p className="font-medium text-sm">{t("settings.schedules.chainSchedule")}</p>
              <p className="text-muted-foreground text-sm">{schedules.chain.schedulePreview}</p>
            </div>
            <div>
              <p className="font-medium text-sm">{t("settings.schedules.nextRun")}</p>
              <p className="text-muted-foreground text-sm">
                {formatNextRun(schedules.chain.nextRun, i18n.language)}
              </p>
            </div>
          </div>
          <Frame>
            {MAINTENANCE_CHAIN_STEPS.map((stepId) => {
              const step = schedules.chain.steps.find((s) => s.id === stepId);
              return (
                <FramePanel key={stepId}>
                  <SettingSwitch
                    label={t(`settings.schedules.chainSteps.${stepId}.title`)}
                    description={t(`settings.schedules.chainSteps.${stepId}.description`)}
                    checked={step?.enabled ?? true}
                    onCheckedChange={(enabled) => updateChainStep(stepId, enabled)}
                  />
                </FramePanel>
              );
            })}
          </Frame>
        </CardPanel>
      </Card>

      <div className="flex flex-col gap-3">
        <h2 className="font-medium text-lg">{t("settings.schedules.otherJobsTitle")}</h2>
        <p className="text-muted-foreground text-sm">{t("settings.schedules.otherJobsDescription")}</p>
        {OTHER_SCHEDULE_JOBS.map((jobId) => {
          const job = schedules.otherJobs.find((j) => j.id === jobId);
          if (!job) {
            return null;
          }
          return (
            <Card key={jobId}>
              <CardPanel className="flex flex-col gap-4">
                <SettingSwitch
                  label={t(`settings.schedules.otherJobs.${jobId}.title`)}
                  description={t(`settings.schedules.otherJobs.${jobId}.description`)}
                  checked={job.enabled}
                  onCheckedChange={(enabled) => updateOtherJob(jobId, { enabled })}
                />
                <div className="grid gap-4 sm:grid-cols-3">
                  <Field>
                    <FieldLabel>{t("settings.schedules.frequency")}</FieldLabel>
                    <Select
                      value={job.frequency}
                      disabled={!job.enabled}
                      onValueChange={(value) =>
                        value && updateOtherJob(jobId, { frequency: value as ScheduleFrequency })
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
                      value={timeDrafts[jobId] ?? job.time}
                      disabled={!job.enabled}
                      onChange={(event) =>
                        setTimeDrafts((current) => ({ ...current, [jobId]: event.target.value }))
                      }
                      onBlur={() => {
                        void commitOtherJobTime(jobId, job.time);
                      }}
                    />
                  </Field>
                  <Field>
                    <FieldLabel>{t("settings.schedules.nextRun")}</FieldLabel>
                    <p className="text-muted-foreground text-sm">
                      {job.enabled
                        ? formatNextRun(job.nextRun, i18n.language)
                        : t("settings.schedules.nextRunUnavailable")}
                    </p>
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
