import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { LoadingBlock } from "@/components/patterns/loading";
import { SettingSwitch } from "@/components/patterns/setting-switch";
import { Button } from "@/components/ui/button";
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
import type { components } from "@/lib/api/client";
import {
  getCatalogSettings,
  getSchedules,
  putCatalogSettings,
  putMaintenanceChainSchedule,
  putScheduledJob,
} from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import {
  MAINTENANCE_CHAIN_STEPS,
  OTHER_SCHEDULE_JOBS,
  type MaintenanceChainStepId,
  type OtherScheduleJobId,
} from "@/lib/maintenance-chain";

type Schedules = components["schemas"]["Schedules"];
type ScheduleFrequency = components["schemas"]["ScheduleFrequency"];
type CatalogSettings = components["schemas"]["CatalogSettings"];
type CatalogSettingsUpdate = components["schemas"]["CatalogSettingsUpdate"];
type CatalogRefreshInterval = components["schemas"]["CatalogRefreshInterval"];

const OTHER_FREQUENCY_OPTIONS: ScheduleFrequency[] = ["daily", "weekly", "monthly"];
// Keyed by the API's enum, so a new interval fails the type check here until
// it has an option.
const CATALOG_INTERVALS = Object.keys({
  off: 0,
  "1h": 0,
  "6h": 0,
  "12h": 0,
  "24h": 0,
} satisfies Record<CatalogRefreshInterval, 0>) as CatalogRefreshInterval[];

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

// Shows what the daemon holds: a value is never displayed as saved until the
// save succeeded and the settings were read back.
function CatalogRefreshCard(): React.ReactElement {
  const { t } = useTranslation();
  const settingsQuery = useApiQuery<CatalogSettings>({
    queryKey: "catalog-settings",
    queryFn: (signal) => getCatalogSettings(signal),
    fallbackError: t("settings.schedules.catalogRefresh.loadFailed"),
  });
  const saveMutation = useApiMutation<CatalogSettingsUpdate, CatalogSettings>({
    mutationFn: (body) => putCatalogSettings(body),
    fallbackError: t("settings.schedules.catalogRefresh.saveFailed"),
  });
  const [saveError, setSaveError] = useState<string | null>(null);
  const settings = settingsQuery.data;

  async function save(update: CatalogSettingsUpdate): Promise<void> {
    setSaveError(null);
    const result = await saveMutation.mutate(update);
    if (!result.ok) {
      if (!result.aborted) {
        setSaveError(result.error);
      }
      return;
    }
    await settingsQuery.refresh();
  }

  const loadError = settingsQuery.error ? (
    <Banner
      tone="error"
      title={t("settings.schedules.catalogRefresh.loadFailed")}
      description={settingsQuery.error}
      action={
        <Button size="xs" variant="outline" onClick={() => void settingsQuery.refresh()}>
          {t("settings.schedules.catalogRefresh.retry")}
        </Button>
      }
    />
  ) : null;

  let body: React.ReactElement | null;
  if (settings === null) {
    body = settingsQuery.error ? null : <LoadingBlock rows={2} />;
  } else {
    body = (
      <>
        {saveError ? (
          <Banner
            tone="error"
            title={t("settings.schedules.catalogRefresh.saveFailed")}
            description={saveError}
          />
        ) : null}
        <Field>
          <FieldLabel>{t("settings.schedules.catalogRefresh.interval")}</FieldLabel>
          <Select
            value={settings.refreshInterval}
            disabled={saveMutation.pending}
            items={CATALOG_INTERVALS.map((interval) => ({
              value: interval,
              label: t(`settings.schedules.catalogRefresh.intervals.${interval}`),
            }))}
            onValueChange={(value) =>
              value && void save({ refreshInterval: value as CatalogRefreshInterval })
            }
          >
            <SelectTrigger className="sm:w-56">
              <SelectValue />
            </SelectTrigger>
            <SelectPopup>
              {CATALOG_INTERVALS.map((interval) => (
                <SelectItem key={interval} value={interval}>
                  {t(`settings.schedules.catalogRefresh.intervals.${interval}`)}
                </SelectItem>
              ))}
            </SelectPopup>
          </Select>
        </Field>
        <SettingSwitch
          label={t("settings.schedules.catalogRefresh.checkOnOpen.label")}
          description={t("settings.schedules.catalogRefresh.checkOnOpen.description")}
          checked={settings.checkOnOpen}
          disabled={saveMutation.pending}
          onCheckedChange={(checkOnOpen) => void save({ checkOnOpen })}
        />
      </>
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("settings.schedules.catalogRefresh.title")}</CardTitle>
      </CardHeader>
      <CardPanel className="flex flex-col gap-4">
        <p className="text-muted-foreground text-sm">{t("settings.schedules.catalogRefresh.description")}</p>
        {loadError}
        {body}
      </CardPanel>
    </Card>
  );
}

export function SchedulesSettingsPage(): React.ReactElement {
  const { t, i18n } = useTranslation();
  const schedulesQuery = useApiQuery<Schedules>({
    queryKey: "schedules",
    queryFn: (signal) => getSchedules(signal),
    fallbackError: t("settings.schedules.loadErrorDescription"),
  });
  const [actionError, setActionError] = useState<string | null>(null);
  const [timeDrafts, setTimeDrafts] = useState<Partial<Record<OtherScheduleJobId, string>>>({});

  const schedules = schedulesQuery.data;

  const chainMutation = useApiMutation<components["schemas"]["MaintenanceChainStep"][], Schedules>({
    mutationFn: (steps) => putMaintenanceChainSchedule(steps),
  });
  const otherJobMutation = useApiMutation<
    { jobId: OtherScheduleJobId; patch: { enabled?: boolean; frequency?: ScheduleFrequency; time?: string } },
    Schedules
  >({
    mutationFn: ({ jobId, patch }) => putScheduledJob(jobId, patch),
  });

  async function updateChainStep(stepId: MaintenanceChainStepId, enabled: boolean): Promise<void> {
    if (!schedules) {
      return;
    }
    setActionError(null);
    const steps = schedules.chain.steps.map((step) =>
      step.id === stepId ? { ...step, enabled } : step,
    );
    const result = await chainMutation.mutate(steps);
    if (!result.ok) {
      if (!result.aborted) {
        setActionError(result.error);
      }
      return;
    }
    await schedulesQuery.refresh();
  }

  async function updateOtherJob(
    jobId: OtherScheduleJobId,
    patch: { enabled?: boolean; frequency?: ScheduleFrequency; time?: string },
  ): Promise<void> {
    if (!schedules) {
      return;
    }
    setActionError(null);
    const result = await otherJobMutation.mutate({ jobId, patch });
    if (!result.ok) {
      if (!result.aborted) {
        setActionError(result.error);
      }
      return;
    }
    await schedulesQuery.refresh();
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

  if (schedulesQuery.loading) {
    return <LoadingBlock />;
  }

  if (!schedules) {
    return (
      <div className="flex flex-col gap-4">
        <Banner
          tone="error"
          title={t("settings.schedules.loadErrorTitle")}
          description={schedulesQuery.error ?? t("settings.schedules.loadErrorDescription")}
        />
        <CatalogRefreshCard />
      </div>
    );
  }

  const error = actionError;

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
                    onCheckedChange={(enabled) => void updateChainStep(stepId, enabled)}
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
                  onCheckedChange={(enabled) => void updateOtherJob(jobId, { enabled })}
                />
                <div className="grid gap-4 sm:grid-cols-3">
                  <Field>
                    <FieldLabel>{t("settings.schedules.frequency")}</FieldLabel>
                    <Select
                      value={job.frequency}
                      disabled={!job.enabled}
                      onValueChange={(value) =>
                        value && void updateOtherJob(jobId, { frequency: value as ScheduleFrequency })
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

      <CatalogRefreshCard />
    </div>
  );
}
