import { Database, HardDrive, Layers, Shield, SkipForward } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { ChoiceCards } from "@/components/patterns/choice-cards";
import { CodeView } from "@/components/patterns/code-view";
import { DataTable } from "@/components/patterns/data-table";
import { JobProgress } from "@/components/patterns/job-progress";
import { NumberUnit } from "@/components/patterns/number-unit";
import { TypedConfirm } from "@/components/patterns/typed-confirm";
import { Wizard } from "@/components/patterns/wizard";
import { Card, CardDescription, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Field } from "@/components/ui/field";
import { hoservaClient, type components } from "@/lib/api/client";
import { buildConfigPreview, formatBytes } from "@/routes/storage-setup/config-preview";
import {
  buildDiscoveryColumns,
  buildRoleColumns,
  discoveryOptionalColumns,
} from "@/routes/storage-setup/columns";
import {
  buildPoolPolicyOptions,
  createPolicyFieldName,
  parseCreatePolicy,
} from "@/routes/storage-setup/pool-options";
import {
  assignableDisks,
  canKeepFilesystem,
  disksToErase,
  roleAssignmentValid,
  typedConfirmMatches,
  validateRoleAssignment,
  type CreatePolicy,
  type DiskEntry,
  type DiskRole,
  type FilesystemChoice,
} from "@/routes/storage-setup/validation";

const STEP_COUNT = 6;
const DEFAULT_MIN_FREE_GB = 50;
const ROLE_UNASSIGNED = "unassigned";
const FORMAT_CHOICE = "format";
const KEEP_CHOICE = "keep";

function filesystemChoiceName(device: string): string {
  return `filesystem-${device}`;
}

const ROLE_ICONS = {
  parity: Shield,
  data: Database,
  cache: Layers,
  ignore: SkipForward,
};

type Job = components["schemas"]["Job"];

function initialRoles(disks: DiskEntry[]): Record<string, DiskRole> {
  const roles: Record<string, DiskRole> = {};
  for (const disk of disks) {
    roles[disk.device] = ROLE_UNASSIGNED;
  }
  return roles;
}

function initialFilesystemChoices(
  disks: DiskEntry[],
  roles: Record<string, DiskRole>,
): Record<string, FilesystemChoice> {
  const choices: Record<string, FilesystemChoice> = {};
  for (const disk of disks) {
    if (roles[disk.device] === "data") {
      choices[disk.device] = FORMAT_CHOICE;
    }
  }
  return choices;
}

export function StorageSetupPage(): React.ReactElement {
  const { t } = useTranslation();
  const [step, setStep] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [poolMounted, setPoolMounted] = useState(false);
  const [disks, setDisks] = useState<DiskEntry[]>([]);
  const [roles, setRoles] = useState<Record<string, DiskRole>>({});
  const [filesystemChoices, setFilesystemChoices] = useState<Record<string, FilesystemChoice>>({});
  const [createPolicy, setCreatePolicy] = useState<CreatePolicy>("mspmfs");
  const [minFreeSpaceGb, setMinFreeSpaceGb] = useState(DEFAULT_MIN_FREE_GB);
  const [confirmText, setConfirmText] = useState("");
  const [activeJob, setActiveJob] = useState<Job | null>(null);
  const [createRequested, setCreateRequested] = useState(false);

  useEffect(() => {
    let cancelled = false;

    Promise.all([
      hoservaClient.GET("/pool", {}),
      hoservaClient.GET("/disks", {}),
    ]).then(([poolResult, disksResult]) => {
      if (cancelled) {
        return;
      }
      if (poolResult.error) {
        setError(poolResult.error.message);
        setLoading(false);
        return;
      }
      if (disksResult.error) {
        setError(disksResult.error.message);
        setLoading(false);
        return;
      }
      const inventory = (disksResult.data?.disks ?? []) as DiskEntry[];
      const nextRoles = initialRoles(inventory);
      setPoolMounted(poolResult.data?.mounted ?? false);
      setDisks(inventory);
      setRoles(nextRoles);
      setFilesystemChoices(initialFilesystemChoices(inventory, nextRoles));
      setLoading(false);
    });

    return () => {
      cancelled = true;
    };
  }, []);

  const validation = useMemo(() => validateRoleAssignment(disks, roles), [disks, roles]);
  const preview = useMemo(
    () =>
      buildConfigPreview({
        disks,
        roles,
        filesystemChoices,
        createPolicy,
        minFreeSpaceGb,
      }),
    [disks, roles, filesystemChoices, createPolicy, minFreeSpaceGb],
  );
  const eraseList = useMemo(() => disksToErase(disks, roles, filesystemChoices), [disks, roles, filesystemChoices]);
  const confirmPhrase = useMemo(() => {
    if (eraseList.length === 0) {
      return "";
    }
    return `erase ${eraseList.join(", ")}`;
  }, [eraseList]);

  const dataDisks = assignableDisks(disks).filter((disk) => roles[disk.device] === "data");
  const optionalColumns = useMemo(() => discoveryOptionalColumns(disks), [disks]);
  const discoveryColumns = useMemo(() => buildDiscoveryColumns(t, optionalColumns), [t, optionalColumns]);
  const poolPolicyOptions = useMemo(() => buildPoolPolicyOptions(t), [t]);

  const roleErrorMessage = useCallback(
    (device: string) => {
      const code = validation.errorsByDevice[device];
      return code ? t(`storageSetup.validation.errors.${code}`) : undefined;
    },
    [t, validation.errorsByDevice],
  );

  const roleWarningMessage = useCallback(
    (device: string) => {
      const code = validation.warningsByDevice[device];
      return code ? t(`storageSetup.validation.warnings.${code}`) : undefined;
    },
    [t, validation.warningsByDevice],
  );

  const setRole = useCallback((device: string, role: DiskRole): void => {
    setRoles((current) => ({ ...current, [device]: role }));
    if (role === "data") {
      setFilesystemChoices((current) => ({ ...current, [device]: current[device] ?? FORMAT_CHOICE }));
      return;
    }
    setFilesystemChoices((current) => {
      const next = { ...current };
      delete next[device];
      return next;
    });
  }, []);

  const roleColumns = useMemo(
    () => buildRoleColumns(t, roles, ROLE_ICONS, setRole, roleErrorMessage, roleWarningMessage),
    [t, roles, setRole, roleErrorMessage, roleWarningMessage],
  );

  const stepMeta = [
    { title: t("storageSetup.steps.discovery.title"), description: t("storageSetup.steps.discovery.description") },
    { title: t("storageSetup.steps.roles.title"), description: t("storageSetup.steps.roles.description") },
    { title: t("storageSetup.steps.filesystem.title"), description: t("storageSetup.steps.filesystem.description") },
    { title: t("storageSetup.steps.pool.title"), description: t("storageSetup.steps.pool.description") },
    { title: t("storageSetup.steps.review.title"), description: t("storageSetup.steps.review.description") },
    { title: t("storageSetup.steps.confirm.title"), description: t("storageSetup.steps.confirm.description") },
  ][step];

  function handleCreate(): void {
    setCreateRequested(true);
    setActiveJob(null);
  }

  function handleNext(): void {
    setError(null);
    if (step < STEP_COUNT - 1) {
      setStep(step + 1);
      return;
    }
    if (!typedConfirmMatches(confirmText, confirmPhrase)) {
      return;
    }
    handleCreate();
  }

  function handleBack(): void {
    setError(null);
    setStep(step - 1);
  }

  const nextDisabled =
    loading ||
    poolMounted ||
    (step === 0 && assignableDisks(disks).length === 0) ||
    (step === 1 && !roleAssignmentValid(validation)) ||
    (step === 5 && !typedConfirmMatches(confirmText, confirmPhrase));

  const nextLabel =
    step === STEP_COUNT - 1 ? t("storageSetup.actions.createArray") : undefined;

  function renderValidationBanner(): React.ReactElement | null {
    if (step !== 1 || (validation.errorCodes.length === 0 && validation.warningCodes.length === 0)) {
      return null;
    }
    if (validation.errorCodes.length > 0) {
      return (
        <Banner
          tone="error"
          title={t("storageSetup.validation.errorsTitle")}
          description={validation.errorCodes.map((code) => t(`storageSetup.validation.errors.${code}`)).join(" ")}
        />
      );
    }
    return (
      <Banner
        tone="warning"
        title={t("storageSetup.validation.warningsTitle")}
        description={validation.warningCodes.map((code) => t(`storageSetup.validation.warnings.${code}`)).join(" ")}
      />
    );
  }

  if (loading) {
    return <p className="text-muted-foreground text-sm">{t("loading.label")}</p>;
  }

  if (poolMounted) {
    return (
      <Banner
        tone="info"
        title={t("storageSetup.arrayExistsTitle")}
        description={t("storageSetup.arrayExistsDescription")}
      />
    );
  }

  return (
    <Wizard
      step={step}
      stepCount={STEP_COUNT}
      title={stepMeta.title}
      description={stepMeta.description}
      onBack={step > 0 ? handleBack : undefined}
      onNext={handleNext}
      nextDisabled={nextDisabled}
      nextLabel={nextLabel}
    >
      {error ? <Banner tone="error" title={error} /> : null}

      {step === 0 ? (
        <DataTable
          rows={disks}
          getRowKey={(disk) => disk.device}
          rowDisabled={(disk) => disk.boot}
          rowDisabledReason={(disk) => (disk.boot ? t("storageSetup.discovery.bootDisk") : undefined)}
          columns={discoveryColumns}
        />
      ) : null}

      {step === 1 ? (
        <div className="flex flex-col gap-4">
          {renderValidationBanner()}
          <DataTable
            rows={assignableDisks(disks)}
            getRowKey={(disk) => disk.device}
            columns={roleColumns}
          />
        </div>
      ) : null}

      {step === 2 ? (
        <div className="flex flex-col gap-4">
          {dataDisks.length === 0 ? (
            <p className="text-muted-foreground text-sm">{t("storageSetup.filesystem.noDataDisks")}</p>
          ) : (
            dataDisks.map((disk) => {
              const choice = filesystemChoices[disk.device] ?? FORMAT_CHOICE;
              const options = [
                {
                  value: FORMAT_CHOICE,
                  title: t("storageSetup.filesystem.formatTitle"),
                  description: t("storageSetup.filesystem.formatDescription", { filesystem: "XFS" }),
                  icon: <HardDrive aria-hidden className="size-5" />,
                },
              ];
              if (canKeepFilesystem(disk)) {
                options.push({
                  value: KEEP_CHOICE,
                  title: t("storageSetup.filesystem.keepTitle"),
                  description: t("storageSetup.filesystem.keepDescription", { filesystem: disk.filesystem }),
                  icon: <Database aria-hidden className="size-5" />,
                });
              }
              return (
                <Card key={disk.device}>
                  <CardHeader>
                    <CardTitle>{disk.device}</CardTitle>
                    <CardDescription>{formatBytes(disk.sizeBytes)}</CardDescription>
                  </CardHeader>
                  <CardPanel>
                    <ChoiceCards
                      name={filesystemChoiceName(disk.device)}
                      value={choice}
                      onChange={(value) =>
                        setFilesystemChoices((current) => ({
                          ...current,
                          [disk.device]: value as FilesystemChoice,
                        }))
                      }
                      options={options}
                    />
                  </CardPanel>
                </Card>
              );
            })
          )}
          <Banner
            tone="info"
            title={t("storageSetup.filesystem.parityTitle")}
            description={t("storageSetup.filesystem.parityDescription")}
          />
        </div>
      ) : null}

      {step === 3 ? (
        <div className="flex flex-col gap-6">
          <Field>
            <p className="mb-2 font-medium">{t("storageSetup.pool.createPolicy")}</p>
            <ChoiceCards
              name={createPolicyFieldName()}
              value={createPolicy}
              onChange={(value) => setCreatePolicy(parseCreatePolicy(value))}
              options={poolPolicyOptions}
            />
          </Field>
          <Field>
            <p className="mb-2 font-medium">{t("storageSetup.pool.minFreeSpace")}</p>
            <NumberUnit
              value={minFreeSpaceGb}
              onChange={setMinFreeSpaceGb}
              unit={t("storageSetup.pool.gigabytes")}
              min={1}
              max={1024}
            />
          </Field>
        </div>
      ) : null}

      {step === 4 ? (
        <div className="flex flex-col gap-4">
          <Card>
            <CardHeader>
              <CardTitle>{t("storageSetup.review.summaryTitle")}</CardTitle>
            </CardHeader>
            <CardPanel className="text-sm">
              <p>{t(`storageSetup.review.faultTolerance.${preview.summary.faultToleranceLabel}`)}</p>
              <p>
                {t("storageSetup.review.summaryLine", {
                  disks: preview.summary.totalDisks,
                  usable: formatBytes(preview.summary.usableBytes),
                  parity: preview.summary.parityDiskCount,
                  paritySize: formatBytes(preview.summary.paritySizeBytes),
                })}
              </p>
            </CardPanel>
          </Card>
          <CodeView title={t("storageSetup.review.snapraidTitle")} code={preview.snapraidConf} />
          <CodeView title={t("storageSetup.review.mergerfsTitle")} code={preview.mergerfsUnit} />
        </div>
      ) : null}

      {step === 5 ? (
        <div className="flex flex-col gap-4">
          <TypedConfirm
            phrase={confirmPhrase}
            value={confirmText}
            onChange={setConfirmText}
            title={t("storageSetup.confirm.eraseTitle")}
            description={t("storageSetup.confirm.eraseDescription")}
            items={eraseList.map((device) => t("storageSetup.confirm.eraseItem", { device }))}
          />
          {createRequested ? (
            activeJob ? (
              <JobProgress job={activeJob} />
            ) : (
              <Banner
                tone="info"
                title={t("storageSetup.confirm.apiPendingTitle")}
                description={t("storageSetup.confirm.apiPendingDescription")}
              />
            )
          ) : null}
        </div>
      ) : null}
    </Wizard>
  );
}
