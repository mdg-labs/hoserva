import { Download, HardDrive, Upload } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { ChoiceCards } from "@/components/patterns/choice-cards";
import { FileUpload } from "@/components/patterns/file-upload";
import { GroupedResults, type ResultGroup } from "@/components/patterns/grouped-results";
import { InlineNote } from "@/components/patterns/inline-note";
import { JobProgress } from "@/components/patterns/job-progress";
import { LoadingBlock } from "@/components/patterns/loading";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Button } from "@/components/ui/button";
import { Field, FieldLabel } from "@/components/ui/field";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { getJobs, getMigrationReport, postJobCancel } from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import { formatBytes } from "@/routes/storage-setup/config-preview";
import {
  RESULT_GROUP_ORDER,
  SOURCE_STICK,
  SOURCE_ZIP,
  groupRows,
  reportRowKey,
  verdictTone,
  type Migration,
  type SourceKind,
  type ReportRow,
} from "@/routes/tools-migrate/report";
import { checkLabel, rowStatusBadge } from "@/routes/tools-migrate/columns";

const SOURCE_FIELD_NAME = "migration-source";
const ZIP_ACCEPT = ".zip,application/zip";
const SCAN_POLL_MS = 2000;
const REPORT_FILE_NAME = "hoserva-migration-report.md";

export function SourceForm({
  migration,
  source,
  onSourceChange,
  onFileChange,
  device,
  onDeviceChange,
  disabled,
}: {
  migration: Migration;
  source: SourceKind;
  onSourceChange: (source: SourceKind) => void;
  onFileChange: (file: File | null) => void;
  device: string;
  onDeviceChange: (device: string) => void;
  disabled: boolean;
}): React.ReactElement {
  const { t } = useTranslation();
  const stickOffered = !migration.zipOnly && migration.flashDevices.length > 0;
  const options = [
    {
      value: SOURCE_ZIP,
      title: t("toolsMigrate.scan.zip.title"),
      description: t("toolsMigrate.scan.zip.description"),
      icon: <Upload aria-hidden className="size-5" />,
    },
    ...(stickOffered
      ? [
          {
            value: SOURCE_STICK,
            title: t("toolsMigrate.scan.stick.title"),
            description: t("toolsMigrate.scan.stick.description"),
            icon: <HardDrive aria-hidden className="size-5" />,
          },
        ]
      : []),
  ];

  return (
    <div className="flex flex-col gap-4">
      {migration.zipOnly ? (
        <InlineNote
          title={t("toolsMigrate.scan.zipOnly.title")}
          description={t("toolsMigrate.scan.zipOnly.description")}
        />
      ) : null}
      {!migration.zipOnly && !stickOffered ? (
        <InlineNote description={t("toolsMigrate.scan.noStick")} />
      ) : null}
      <ChoiceCards
        name={SOURCE_FIELD_NAME}
        value={stickOffered ? source : SOURCE_ZIP}
        onChange={(value) => onSourceChange(value === SOURCE_STICK ? SOURCE_STICK : SOURCE_ZIP)}
        options={options}
      />
      {stickOffered && source === SOURCE_STICK ? (
        <Field>
          <FieldLabel>{t("toolsMigrate.scan.stick.device")}</FieldLabel>
          <Select value={device} onValueChange={(value) => value && onDeviceChange(value)} disabled={disabled}>
            <SelectTrigger aria-label={t("toolsMigrate.scan.stick.device")}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {migration.flashDevices.map((flash) => (
                <SelectItem key={flash.device} value={flash.device}>
                  {t("toolsMigrate.scan.stick.deviceOption", {
                    device: flash.device,
                    model: flash.model ?? t("toolsMigrate.scan.stick.unknownModel"),
                    size: formatBytes(flash.size),
                  })}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>
      ) : (
        <FileUpload
          id="migration-flash-backup"
          label={t("toolsMigrate.scan.zip.label")}
          description={t("toolsMigrate.scan.zip.hint")}
          accept={ZIP_ACCEPT}
          disabled={disabled}
          onChange={onFileChange}
        />
      )}
    </div>
  );
}

export function ScanProgress({ onChanged }: { onChanged: () => void }): React.ReactElement {
  const { t } = useTranslation();
  const jobs = useApiQuery({
    queryKey: "migration-scan-job",
    queryFn: (signal) => getJobs({ class: "topology", limit: 20 }, signal),
    pollIntervalMs: SCAN_POLL_MS,
    fallbackError: t("toolsMigrate.scan.jobLoadFailed"),
  });
  const cancel = useApiMutation({
    mutationFn: postJobCancel,
    fallbackError: t("toolsMigrate.scan.cancelFailed"),
  });
  const job = jobs.data?.jobs.find(
    (candidate) => candidate.type === "migration_scan" && (candidate.status === "queued" || candidate.status === "running"),
  );

  async function handleCancel(jobId: string): Promise<void> {
    const result = await cancel.mutate(jobId);
    if (result.ok) {
      onChanged();
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <InlineNote description={t("toolsMigrate.scan.running")} />
      {jobs.error ? <Banner tone="error" title={jobs.error} /> : null}
      {cancel.error ? <Banner tone="error" title={cancel.error} /> : null}
      {job ? <JobProgress job={job} onCancel={(jobId) => void handleCancel(jobId)} /> : <LoadingBlock rows={1} />}
    </div>
  );
}

const REVOKE_DELAY_MS = 10_000;

function saveText(text: string, filename: string): void {
  const url = URL.createObjectURL(new Blob([text], { type: "text/markdown" }));
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  setTimeout(() => URL.revokeObjectURL(url), REVOKE_DELAY_MS);
}

function ReportRowItem({ row }: { row: ReportRow }): React.ReactElement {
  const { t } = useTranslation();

  return (
    <span className="flex flex-wrap items-baseline gap-x-2">
      {rowStatusBadge(t, row.status)}
      <span className="font-medium text-foreground">
        {row.subject ? `${checkLabel(t, row.check)} · ${row.subject}` : checkLabel(t, row.check)}
      </span>
      <span>{row.detail}</span>
    </span>
  );
}

export function ScanReport({
  migration,
  onScanAgain,
}: {
  migration: Migration;
  onScanAgain: () => void;
}): React.ReactElement | null {
  const { t, i18n } = useTranslation();
  const [downloadError, setDownloadError] = useState<string | null>(null);
  const download = useApiMutation({
    mutationFn: (signal?: AbortSignal) => getMigrationReport(signal),
    fallbackError: t("toolsMigrate.scan.report.downloadFailed"),
  });
  const report = migration.report;
  const grouped = useMemo(() => groupRows(report?.rows ?? []), [report]);
  const groups = useMemo<ResultGroup[]>(
    () =>
      RESULT_GROUP_ORDER.filter((id) => grouped[id].length > 0).map((id) => ({
        id,
        label: t(`toolsMigrate.scan.report.groups.${id}`),
        count: grouped[id].length,
        defaultOpen: id !== "pass",
        items: grouped[id].map((row) => <ReportRowItem key={reportRowKey(row)} row={row} />),
      })),
    [grouped, t],
  );

  if (!report) {
    return null;
  }

  async function handleDownload(): Promise<void> {
    setDownloadError(null);
    const result = await download.mutate(undefined);
    if (!result.ok) {
      return;
    }
    if (result.data === undefined) {
      setDownloadError(t("toolsMigrate.scan.report.downloadFailed"));
      return;
    }
    saveText(result.data, REPORT_FILE_NAME);
  }

  const generated = new Intl.DateTimeFormat(i18n.language, { dateStyle: "medium", timeStyle: "short" }).format(
    new Date(report.generatedAt),
  );
  const sourceText = migration.sourceDevice
    ? t("toolsMigrate.scan.report.fromStick", { device: migration.sourceDevice })
    : migration.sourceSize !== undefined
      ? t("toolsMigrate.scan.report.fromZipSize", { size: formatBytes(migration.sourceSize) })
      : t("toolsMigrate.scan.report.fromZip");

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-2">
        <StatusBadge tone={verdictTone(report.verdict)}>{t(`toolsMigrate.scan.report.verdict.${report.verdict}`, { defaultValue: report.verdict })}</StatusBadge>
        <span className="text-muted-foreground text-sm">
          {t("toolsMigrate.scan.report.generated", { date: generated, source: sourceText })}
        </span>
      </div>
      <p className="text-sm">{t(`toolsMigrate.scan.report.verdictHelp.${report.verdict}`, { defaultValue: "" })}</p>
      {download.error ?? downloadError ? <Banner tone="error" title={download.error ?? downloadError ?? ""} /> : null}
      <GroupedResults groups={groups} />
      <div className="flex flex-wrap gap-2">
        <Button variant="outline" loading={download.pending} onClick={() => void handleDownload()}>
          <Download aria-hidden />
          {t("toolsMigrate.scan.report.download")}
        </Button>
        <Button variant="ghost" onClick={onScanAgain}>
          {t("toolsMigrate.actions.scanAgain")}
        </Button>
      </div>
    </div>
  );
}
