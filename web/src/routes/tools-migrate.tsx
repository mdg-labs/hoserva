import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { LoadingBlock } from "@/components/patterns/loading";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Wizard } from "@/components/patterns/wizard";
import { Button } from "@/components/ui/button";
import type { components } from "@/lib/api/client";
import {
  getDisks,
  getMigration,
  initializeMigrationParity,
  startMigrationDeviceScan,
  startMigrationImport,
  startMigrationScan,
  startMigrationVerify,
} from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import { UnprotectedWindowBanner, UnverifiedLayoutBanner } from "@/routes/tools-migrate/banners";
import { ImportConfirm, type PartitionChoice, type PartitionOptions } from "@/routes/tools-migrate/import-confirm";
import { ImportStep } from "@/routes/tools-migrate/import-step";
import { anyActive, initialSyncOf, useInitialSyncJobs, useMigrationJobs } from "@/routes/tools-migrate/jobs";
import {
  importRoles,
  needsCachePartition,
  proposedMapping,
  type DiskMapping,
  type MappingRole,
} from "@/routes/tools-migrate/mapping";
import { ParityConfirm, ParityProgress } from "@/routes/tools-migrate/parity-step";
import {
  SOURCE_STICK,
  SOURCE_ZIP,
  jobActive,
  latestJob,
  verifyGreen,
  type Job,
  type Migration,
  type SourceKind,
} from "@/routes/tools-migrate/report";
import { ReviewStep } from "@/routes/tools-migrate/review-step";
import { ScanProgress, ScanReport, SourceForm } from "@/routes/tools-migrate/scan-step";
import { VerifyStep } from "@/routes/tools-migrate/verify-step";

const STEP_KEYS = ["scan", "review", "import", "verify"] as const;
const SCAN_STEP = 0;
const REVIEW_STEP = 1;
const IMPORT_STEP = 2;
const VERIFY_STEP = 3;
const POLL_MS = 2000;
const JOB_PHASES: Array<Migration["phase"]> = ["scanned", "imported", "verifying", "verify_failed", "verified", "initializing"];
const ADOPTED_PHASES: Array<Migration["phase"]> = ["imported", "verifying", "verify_failed", "verified", "initializing"];
const DONE_ROUTE = "/";

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
        </li>
      ))}
    </ol>
  );
}

function partitionChoices(disks: DiskInventory, device: string | undefined): PartitionChoice[] {
  return disks
    .filter((disk) => disk.boot && disk.device === device)
    .flatMap((disk) => disk.cachePartitions ?? [])
    .flatMap((partition) =>
      partition.byIdName && partition.partUuid
        ? [
            {
              device: partition.device,
              byIdName: partition.byIdName,
              partUuid: partition.partUuid,
              sizeBytes: partition.sizeBytes,
            },
          ]
        : [],
    );
}

type DiskInventory = Array<components["schemas"]["DiskInventoryEntry"]>;

export function ToolsMigratePage(): React.ReactElement {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [viewStep, setViewStep] = useState<number | null>(null);
  const [rescan, setRescan] = useState(false);
  const [source, setSource] = useState<SourceKind>(SOURCE_ZIP);
  const [zipFile, setZipFile] = useState<File | null>(null);
  const [selectedDevice, setSelectedDevice] = useState<string | null>(null);
  const [polling, setPolling] = useState(false);
  const [syncPolling, setSyncPolling] = useState(false);
  const [startedFrom, setStartedFrom] = useState<Migration | null>(null);
  // The roles edited so far, kept for the report they were edited against: a
  // new scan starts again from the proposed roles.
  const [edited, setEdited] = useState<{ reportAt: string; roles: DiskMapping } | null>(null);
  // The report whose mapping the user has confirmed; changing a role or the
  // partition takes the confirmation back.
  const [confirmedFor, setConfirmedFor] = useState<string | null>(null);
  const [partitionDevice, setPartitionDevice] = useState("");
  const [confirmText, setConfirmText] = useState("");
  const [busy, setBusy] = useState(false);

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
  const importRequest = useApiMutation({
    mutationFn: startMigrationImport,
    fallbackError: t("toolsMigrate.import.startFailed"),
  });
  const verifyRequest = useApiMutation({
    mutationFn: startMigrationVerify,
    fallbackError: t("toolsMigrate.verify.startFailed"),
  });
  const parityRequest = useApiMutation({
    mutationFn: initializeMigrationParity,
    fallbackError: t("toolsMigrate.parity.startFailed"),
  });

  // After a scan starts the session read before it is out of date: if the
  // reload fails, that read must not stand in for the current state.
  const reloadFailed = migrationQuery.error !== null && migrationQuery.data === startedFrom;
  const migration = reloadFailed ? null : migrationQuery.data;
  const phase = migration?.phase;
  const scanning = phase === "scanning";

  const jobsEnabled = phase !== undefined && JOB_PHASES.includes(phase);
  const jobsQuery = useMigrationJobs(jobsEnabled, polling);
  const jobList = jobsQuery.data?.jobs ?? [];
  const importJob = latestJob(jobList, "migration_import");
  const verifyJob = latestJob(jobList, "migration_verify");
  const parityJob = latestJob(jobList, "migration_parity");
  const importing = jobActive(importJob);
  const verifying = phase === "verifying" || jobActive(verifyJob);
  const importFailed = importJob !== undefined && !importing && importJob.status !== "succeeded";
  // The point of no return leaves no pending import behind, so a migration that
  // went through it is told apart by its job, not by the session's phase.
  const pastNoReturn =
    jobsEnabled && parityJob !== undefined && (jobActive(parityJob) || parityJob.status === "succeeded");
  const syncEnabled = pastNoReturn && parityJob?.status === "succeeded";
  const syncJobs = useInitialSyncJobs(syncEnabled, syncPolling);
  const syncJob = syncEnabled && parityJob ? initialSyncOf(syncJobs.data?.jobs ?? [], parityJob) : undefined;
  // The unprotected window stays open until a sync has succeeded, so a sync the
  // schedule runs later is found whether the shown one is running, failed or absent.
  const wantSyncPolling = syncEnabled && syncJob?.status !== "succeeded";
  if (wantSyncPolling !== syncPolling) {
    setSyncPolling(wantSyncPolling);
  }

  const awaitingImport = viewStep === IMPORT_STEP && phase === "scanned" && !importFailed;
  const wantPolling =
    scanning || phase === "verifying" || anyActive(importJob, verifyJob, parityJob) || awaitingImport;
  if (wantPolling !== polling) {
    setPolling(wantPolling);
  }
  // A scan the page did not start (a reload mid-scan) ends on its report, not
  // on the review the finished phase would otherwise resume at.
  if (scanning && viewStep === null) {
    setViewStep(SCAN_STEP);
  }

  const report = migration?.report;
  const review = report?.review;
  const proposed = review ? proposedMapping(review.disks) : undefined;
  const mapping =
    proposed && edited && edited.reportAt === report?.generatedAt ? { ...proposed, ...edited.roles } : proposed;
  const cacheRow = review && mapping ? needsCachePartition(review.disks, mapping) : undefined;
  const disksQuery = useApiQuery({
    queryKey: "migration-disks",
    queryFn: (signal) => getDisks(signal),
    enabled: phase === "scanned" && cacheRow !== undefined,
    fallbackError: t("toolsMigrate.review.import.partition.loadFailed"),
  });

  if (migrationQuery.loading || (jobsEnabled && jobsQuery.data === null && jobsQuery.error === null)) {
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

  const reviewable = phase === "scanned" && report !== undefined;
  const adopted = phase !== undefined && ADOPTED_PHASES.includes(phase);
  let step = SCAN_STEP;
  if (pastNoReturn) {
    step = VERIFY_STEP;
  } else if (adopted) {
    const resumeAt = importing || (phase === "imported" && !verifying) ? IMPORT_STEP : VERIFY_STEP;
    step = importing ? IMPORT_STEP : Math.max(viewStep ?? resumeAt, IMPORT_STEP);
  } else if (phase === "scanned") {
    step = importing || viewStep === IMPORT_STEP ? IMPORT_STEP : reviewable ? (viewStep ?? REVIEW_STEP) : SCAN_STEP;
  }
  const stickOffered = !migration.zipOnly && migration.flashDevices.length > 0;
  const effectiveSource: SourceKind = stickOffered ? source : SOURCE_ZIP;
  const device = migration.flashDevices.some((flash) => flash.device === selectedDevice)
    ? (selectedDevice ?? "")
    : (migration.flashDevices[0]?.device ?? "");
  const startError = effectiveSource === SOURCE_STICK ? deviceScan.error : zipScan.error;
  const startPending = zipScan.pending || deviceScan.pending;
  const showReport = step === SCAN_STEP && reviewable && !rescan;
  const showForm = step === SCAN_STEP && !scanning && !showReport;

  const partitions = cacheRow ? partitionChoices(disksQuery.data?.disks ?? [], cacheRow.device) : [];
  const chosenPartition = partitions.find((partition) => partition.device === partitionDevice) ?? null;
  const planned = review && mapping ? importRoles(review.disks, mapping, chosenPartition) : null;
  const noGo = report?.verdict === "no_go";
  const confirmed = report !== undefined && confirmedFor === report.generatedAt;
  const importReady = planned !== null && planned.blocker === null && confirmed && !noGo;
  const partitionOptions: PartitionOptions | null = cacheRow
    ? {
        loading: disksQuery.loading,
        error: disksQuery.error,
        partitions,
        selected: chosenPartition?.device ?? "",
        onSelect: (value) => {
          setPartitionDevice(value);
          setConfirmedFor(null);
        },
      }
    : null;

  const parityInit = migration.parityInit;
  const verifyIsGreen = migration.verify !== undefined && verifyGreen(migration.verify);
  const offerParity = !pastNoReturn && ((phase === "verified" && verifyIsGreen) || phase === "initializing");
  const confirmation = parityInit?.confirmation;
  const parityReady =
    offerParity && parityInit?.problem === undefined && confirmation !== undefined && confirmText === confirmation;
  const failedParityJob =
    parityJob !== undefined && !jobActive(parityJob) && parityJob.status !== "succeeded" ? parityJob : undefined;

  async function refreshAll(): Promise<void> {
    await Promise.all([migrationQuery.refresh(), jobsQuery.refresh()]);
  }

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

  async function handleImport(): Promise<void> {
    if (!planned || !importReady) {
      return;
    }
    importRequest.reset();
    setBusy(true);
    try {
      const result = await importRequest.mutate({ roles: planned.roles, confirm: true });
      if (!result.ok) {
        return;
      }
      await refreshAll();
      setViewStep(IMPORT_STEP);
    } finally {
      setBusy(false);
    }
  }

  async function handleVerify(): Promise<void> {
    verifyRequest.reset();
    setBusy(true);
    try {
      await verifyRequest.mutate(undefined);
      await refreshAll();
      setViewStep(VERIFY_STEP);
    } finally {
      setBusy(false);
    }
  }

  async function handleInitialize(): Promise<void> {
    if (!parityReady || confirmation === undefined) {
      return;
    }
    parityRequest.reset();
    setBusy(true);
    try {
      const result = await parityRequest.mutate(confirmation);
      await refreshAll();
      if (result.ok) {
        setConfirmText("");
      }
    } finally {
      setBusy(false);
    }
  }

  function handleMappingChange(key: string, role: MappingRole | null): void {
    if (report) {
      setEdited({ reportAt: report.generatedAt, roles: { ...mapping, [key]: role } });
      setConfirmedFor(null);
    }
  }

  function handleScanAgain(): void {
    setViewStep(SCAN_STEP);
    setRescan(true);
    setZipFile(null);
    setConfirmedFor(null);
  }

  function handleSourceChange(next: SourceKind): void {
    setSource(next);
    setZipFile(null);
  }

  const canStart = effectiveSource === SOURCE_STICK ? device !== "" : zipFile !== null;
  const stepKey = STEP_KEYS[step];
  const syncSucceeded = syncJob?.status === "succeeded";

  let nextLabel = showReport ? t("toolsMigrate.actions.review") : t("toolsMigrate.actions.startScan");
  let nextDisabled = scanning || startPending || (showForm && !canStart);
  let nextLoading = startPending || scanning;
  let onNext: () => void = () => {
    if (showReport) {
      setViewStep(REVIEW_STEP);
      return;
    }
    void handleStartScan();
  };
  let onBack: (() => void) | undefined = showForm && rescan ? () => setRescan(false) : undefined;
  if (step === REVIEW_STEP) {
    nextLabel = t("toolsMigrate.actions.import");
    nextDisabled = !importReady || busy;
    nextLoading = busy;
    onNext = () => void handleImport();
    onBack = () => setViewStep(SCAN_STEP);
  } else if (step === IMPORT_STEP) {
    nextLabel = t("toolsMigrate.actions.continueToVerify");
    nextDisabled = !adopted || importing;
    nextLoading = importing;
    onNext = () => setViewStep(VERIFY_STEP);
    onBack = phase === "scanned" && !importing ? () => setViewStep(REVIEW_STEP) : undefined;
  } else if (step === VERIFY_STEP) {
    onBack = undefined;
    if (pastNoReturn) {
      nextLabel = t("toolsMigrate.actions.done");
      nextDisabled = !syncSucceeded;
      nextLoading = false;
      onNext = () => void navigate(DONE_ROUTE);
    } else if (offerParity) {
      nextLabel = t(parityInit?.finishing ? "toolsMigrate.actions.finishParity" : "toolsMigrate.actions.initializeParity");
      nextDisabled = !parityReady || busy;
      nextLoading = busy;
      onNext = () => void handleInitialize();
    } else {
      nextLabel = t(migration.verify || verifyJob ? "toolsMigrate.actions.rerunVerify" : "toolsMigrate.actions.verify");
      nextDisabled = verifying || busy;
      nextLoading = verifying || busy;
      onNext = () => void handleVerify();
    }
  }

  const onJobChanged = (): void => void refreshAll();

  return (
    <div className="flex flex-col gap-4">
      <StepList current={step} />
      <Wizard
        step={step}
        stepCount={STEP_KEYS.length}
        title={t(`toolsMigrate.steps.${stepKey}`)}
        description={t(`toolsMigrate.steps.${stepKey}Description`)}
        onBack={onBack}
        onNext={onNext}
        nextLabel={nextLabel}
        nextDisabled={nextDisabled}
        nextLoading={nextLoading}
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
          {jobsQuery.error ? (
            <Banner
              tone="error"
              title={jobsQuery.error}
              action={
                <Button size="xs" variant="outline" onClick={() => void jobsQuery.refresh()}>
                  {t("toolsMigrate.retry")}
                </Button>
              }
            />
          ) : null}
          {report?.unverifiedLayout && !scanning ? <UnverifiedLayoutBanner /> : null}
          {step >= REVIEW_STEP && !syncSucceeded ? <UnprotectedWindowBanner /> : null}

          {phase === "scan_failed" ? (
            <Banner
              tone="error"
              title={t("toolsMigrate.scan.failedTitle")}
              description={migration.scanError ?? t("toolsMigrate.scan.failedUnknown")}
            />
          ) : null}
          {startError ? <Banner tone="error" title={startError} /> : null}
          {importRequest.error && step === REVIEW_STEP ? (
            <Banner tone="error" title={t("toolsMigrate.import.refusedTitle")} description={importRequest.error} />
          ) : null}
          {verifyRequest.error && step === VERIFY_STEP ? <Banner tone="error" title={verifyRequest.error} /> : null}

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
          {step === REVIEW_STEP && report ? (
            <>
              <ReviewStep
                report={report}
                mapping={mapping}
                onMappingChange={handleMappingChange}
                onScanAgain={handleScanAgain}
              />
              <ImportConfirm
                confirmed={confirmed}
                onConfirmedChange={(value) => setConfirmedFor(value ? report.generatedAt : null)}
                blocker={planned?.blocker ?? null}
                noGo={noGo}
                partition={partitionOptions}
              />
            </>
          ) : null}
          {step === IMPORT_STEP ? <ImportStep adopted={adopted} job={importJob} onChanged={onJobChanged} /> : null}
          {step === VERIFY_STEP ? (
            <VerifyBody
              pastNoReturn={pastNoReturn}
              parityJob={parityJob}
              syncJob={syncJob}
              syncError={syncJobs.error}
              syncLoading={syncJobs.data === null && syncJobs.error === null}
              phase={phase}
              migration={migration}
              verifyJob={verifyJob}
              verifying={verifying}
              parityConfirm={
                <ParityConfirm
                  init={parityInit}
                  value={confirmText}
                  onChange={setConfirmText}
                  refusal={parityRequest.error}
                  failedJob={failedParityJob}
                />
              }
              onChanged={onJobChanged}
            />
          ) : null}
        </div>
      </Wizard>
      <p className="mx-auto w-full max-w-2xl text-muted-foreground text-xs">{t("toolsMigrate.trademark")}</p>
    </div>
  );
}

function VerifyBody({
  pastNoReturn,
  parityJob,
  syncJob,
  syncError,
  syncLoading,
  phase,
  migration,
  verifyJob,
  verifying,
  parityConfirm,
  onChanged,
}: {
  pastNoReturn: boolean;
  parityJob: Job | undefined;
  syncJob: Job | undefined;
  syncError: string | null;
  syncLoading: boolean;
  phase: Migration["phase"] | undefined;
  migration: Migration;
  verifyJob: Job | undefined;
  verifying: boolean;
  parityConfirm: React.ReactNode;
  onChanged: () => void;
}): React.ReactElement | null {
  if (pastNoReturn && parityJob) {
    return (
      <ParityProgress
        parityJob={parityJob}
        syncJob={syncJob}
        syncError={syncError}
        syncLoading={syncLoading}
        onChanged={onChanged}
      />
    );
  }
  if (phase === "initializing") {
    return <>{parityConfirm}</>;
  }
  return (
    <VerifyStep verify={migration.verify} job={verifyJob} running={verifying} onChanged={onChanged}>
      {phase === "verified" ? parityConfirm : null}
    </VerifyStep>
  );
}
