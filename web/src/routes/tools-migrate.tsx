import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { LoadingBlock } from "@/components/patterns/loading";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Wizard } from "@/components/patterns/wizard";
import { Button } from "@/components/ui/button";
import { getMigration, startMigrationDeviceScan, startMigrationScan } from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import { UnprotectedWindowBanner, UnverifiedLayoutBanner } from "@/routes/tools-migrate/banners";
import { ReviewStep } from "@/routes/tools-migrate/review-step";
import { SOURCE_STICK, SOURCE_ZIP, type Migration, type SourceKind } from "@/routes/tools-migrate/report";
import { ScanProgress, ScanReport, SourceForm } from "@/routes/tools-migrate/scan-step";

const STEP_KEYS = ["scan", "review", "import", "verify"] as const;
const SCAN_STEP = 0;
const REVIEW_STEP = 1;
const FIRST_LATER_STEP = 2;
const POLL_MS = 2000;

function StepList({ current }: { current: number }): React.ReactElement {
  const { t } = useTranslation();

  return (
    <ol className="mx-auto flex w-full max-w-2xl flex-wrap gap-2" aria-label={t("toolsMigrate.steps.ariaLabel")}>
      {STEP_KEYS.map((key, index) => (
        <li key={key} className="flex items-center gap-2 text-sm" aria-current={index === current ? "step" : undefined}>
          <span className={index === current ? "font-medium" : "text-muted-foreground"}>
            {index + 1}. {t(`toolsMigrate.steps.${key}`)}
          </span>
          {index === current ? <StatusBadge tone="info">{t("toolsMigrate.steps.current")}</StatusBadge> : null}
          {index >= FIRST_LATER_STEP ? <StatusBadge tone="outline">{t("toolsMigrate.steps.later")}</StatusBadge> : null}
        </li>
      ))}
    </ol>
  );
}

export function ToolsMigratePage(): React.ReactElement {
  const { t } = useTranslation();
  const [viewStep, setViewStep] = useState<number | null>(null);
  const [rescan, setRescan] = useState(false);
  const [source, setSource] = useState<SourceKind>(SOURCE_ZIP);
  const [zipFile, setZipFile] = useState<File | null>(null);
  const [selectedDevice, setSelectedDevice] = useState<string | null>(null);
  const [polling, setPolling] = useState(false);
  const [startedFrom, setStartedFrom] = useState<Migration | null>(null);

  const migrationQuery = useApiQuery({
    queryKey: "migration",
    queryFn: (signal) => getMigration(signal),
    pollIntervalMs: polling ? POLL_MS : undefined,
    fallbackError: t("toolsMigrate.loadFailed"),
  });
  const zipScan = useApiMutation({ mutationFn: startMigrationScan, fallbackError: t("toolsMigrate.scan.startFailed") });
  const deviceScan = useApiMutation({
    mutationFn: startMigrationDeviceScan,
    fallbackError: t("toolsMigrate.scan.startFailed"),
  });

  // After a scan starts the session read before it is out of date: if the
  // reload fails, that read must not stand in for the current state.
  const reloadFailed = migrationQuery.error !== null && migrationQuery.data === startedFrom;
  const migration = reloadFailed ? null : migrationQuery.data;
  const phase = migration?.phase;
  const scanning = phase === "scanning";
  if (scanning !== polling) {
    setPolling(scanning);
  }
  // A scan the page did not start (a reload mid-scan) ends on its report, not
  // on the review the finished phase would otherwise resume at.
  if (scanning && viewStep === null) {
    setViewStep(SCAN_STEP);
  }

  if (migrationQuery.loading) {
    return <LoadingBlock rows={3} />;
  }
  if (!migration) {
    return (
      <Banner
        tone="error"
        title={migrationQuery.error ?? t("toolsMigrate.loadFailed")}
        action={
          <Button size="xs" variant="outline" onClick={() => void migrationQuery.refresh()}>
            {t("toolsMigrate.retry")}
          </Button>
        }
      />
    );
  }

  const report = migration.report;
  const reviewable = phase === "scanned" && report !== undefined;
  const step = reviewable ? (viewStep ?? REVIEW_STEP) : SCAN_STEP;
  const stickOffered = !migration.zipOnly && migration.flashDevices.length > 0;
  const effectiveSource: SourceKind = stickOffered ? source : SOURCE_ZIP;
  const device = migration.flashDevices.some((flash) => flash.device === selectedDevice)
    ? (selectedDevice ?? "")
    : (migration.flashDevices[0]?.device ?? "");
  const startError = effectiveSource === SOURCE_STICK ? deviceScan.error : zipScan.error;
  const startPending = zipScan.pending || deviceScan.pending;
  const showReport = step === SCAN_STEP && reviewable && !rescan;
  const showForm = step === SCAN_STEP && !scanning && !showReport;

  async function handleStartScan(): Promise<void> {
    zipScan.reset();
    deviceScan.reset();
    const result =
      effectiveSource === SOURCE_STICK
        ? await deviceScan.mutate(device)
        : zipFile
          ? await zipScan.mutate(zipFile)
          : null;
    if (!result?.ok) {
      return;
    }
    setViewStep(SCAN_STEP);
    setRescan(false);
    setZipFile(null);
    setStartedFrom(migrationQuery.data);
    await migrationQuery.refresh();
  }

  function handleNext(): void {
    if (step === REVIEW_STEP) {
      return;
    }
    if (showReport) {
      setViewStep(REVIEW_STEP);
      return;
    }
    void handleStartScan();
  }

  function handleBack(): void {
    if (step === REVIEW_STEP) {
      setViewStep(SCAN_STEP);
      return;
    }
    setRescan(false);
  }

  function handleScanAgain(): void {
    setViewStep(SCAN_STEP);
    setRescan(true);
    setZipFile(null);
  }

  function handleSourceChange(next: SourceKind): void {
    setSource(next);
    setZipFile(null);
  }

  const canStart = effectiveSource === SOURCE_STICK ? device !== "" : zipFile !== null;
  const nextLabel = step === REVIEW_STEP ? t("toolsMigrate.actions.import") : showReport ? t("toolsMigrate.actions.review") : t("toolsMigrate.actions.startScan");
  const stepKey = STEP_KEYS[step];

  return (
    <div className="flex flex-col gap-4">
      <StepList current={step} />
      <Wizard
        step={step}
        stepCount={STEP_KEYS.length}
        title={t(`toolsMigrate.steps.${stepKey}`)}
        description={t(`toolsMigrate.steps.${stepKey}Description`)}
        onBack={step === REVIEW_STEP || (showForm && rescan) ? handleBack : undefined}
        onNext={handleNext}
        nextLabel={nextLabel}
        nextDisabled={step === REVIEW_STEP || scanning || startPending || (showForm && !canStart)}
        nextLoading={startPending || scanning}
      >
        <div className="flex flex-col gap-4">
          {migrationQuery.error ? (
            <Banner
              tone="error"
              title={migrationQuery.error}
              action={
                <Button size="xs" variant="outline" onClick={() => void migrationQuery.refresh()}>
                  {t("toolsMigrate.retry")}
                </Button>
              }
            />
          ) : null}
          {report?.unverifiedLayout && !scanning ? <UnverifiedLayoutBanner /> : null}
          {step >= REVIEW_STEP ? <UnprotectedWindowBanner /> : null}

          {phase === "scan_failed" ? (
            <Banner
              tone="error"
              title={t("toolsMigrate.scan.failedTitle")}
              description={migration.scanError ?? t("toolsMigrate.scan.failedUnknown")}
            />
          ) : null}
          {startError ? <Banner tone="error" title={startError} /> : null}

          {scanning ? <ScanProgress onChanged={() => void migrationQuery.refresh()} /> : null}
          {showForm ? (
            <SourceForm
              migration={migration}
              source={effectiveSource}
              onSourceChange={handleSourceChange}
              onFileChange={setZipFile}
              device={device}
              onDeviceChange={setSelectedDevice}
              disabled={startPending}
            />
          ) : null}
          {showReport ? <ScanReport migration={migration} onScanAgain={handleScanAgain} /> : null}
          {step === REVIEW_STEP && report ? <ReviewStep rows={report.rows} onScanAgain={handleScanAgain} /> : null}
        </div>
      </Wizard>
      <p className="mx-auto w-full max-w-2xl text-muted-foreground text-xs">{t("toolsMigrate.trademark")}</p>
    </div>
  );
}
