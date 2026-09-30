import { Archive } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { ConfirmDialog } from "@/components/patterns/confirm";
import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { EmptyState } from "@/components/patterns/empty-state";
import { showFeedbackToast } from "@/components/patterns/feedback-toast";
import { FormOverlay } from "@/components/patterns/form-overlay";
import { jobStatusLabel } from "@/components/patterns/job-status";
import { LoadingBlock } from "@/components/patterns/loading";
import { NumberUnit } from "@/components/patterns/number-unit";
import { SecretInput } from "@/components/patterns/secret-input";
import { SettingSwitch } from "@/components/patterns/setting-switch";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Button } from "@/components/ui/button";
import { Card, CardAction, CardDescription, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select, SelectItem, SelectPopup, SelectTrigger, SelectValue } from "@/components/ui/select";
import { jobDetailPath } from "@/hooks/paths";
import type { components } from "@/lib/api/client";
import {
  deleteBackupDestination,
  getBackupDestinations,
  getGeneralSettings,
  getRestoreDrill,
  getSchedules,
  postBackupDestination,
  postBackupDestinationTest,
  postConfigExport,
  postRestoreDrill,
  putGeneralSettings,
  type Availability,
} from "@/lib/api/operations";
import { parseClientResult, type ClientResult } from "@/lib/api/request";
import { useApiQuery, type UseApiQueryResult } from "@/lib/api/use-api-query";

type BackupDestination = components["schemas"]["BackupDestination"];
type BackupDestinationType = components["schemas"]["BackupDestinationType"];
type BackupDestinationTestResult = components["schemas"]["BackupDestinationTestResult"];
type CreateBackupDestinationRequest = components["schemas"]["CreateBackupDestinationRequest"];
type GeneralSettings = components["schemas"]["GeneralSettings"];
type Job = components["schemas"]["Job"];
type RestoreDrill = components["schemas"]["RestoreDrill"];
type Schedules = components["schemas"]["Schedules"];

type DestinationsLoad = Availability<{ destinations: BackupDestination[] }>;

const CODE_PASSPHRASE_REQUIRED = "backup_passphrase_required";
const CODE_RCLONE_MISSING = "rclone_missing";

const DESTINATION_TYPES: BackupDestinationType[] = ["local", "smb", "s3", "sftp", "webdav", "rclone"];

type FieldSpec = { key: string; secret: boolean; required: boolean };

const REMOTE_FIELDS: Record<Exclude<BackupDestinationType, "local">, FieldSpec[]> = {
  smb: [
    { key: "host", secret: false, required: true },
    { key: "user", secret: false, required: true },
    { key: "port", secret: false, required: false },
    { key: "domain", secret: false, required: false },
    { key: "pass", secret: true, required: true },
  ],
  s3: [
    { key: "access_key_id", secret: false, required: true },
    { key: "provider", secret: false, required: false },
    { key: "endpoint", secret: false, required: false },
    { key: "region", secret: false, required: false },
    { key: "secret_access_key", secret: true, required: true },
  ],
  sftp: [
    { key: "host", secret: false, required: true },
    { key: "user", secret: false, required: true },
    { key: "port", secret: false, required: false },
    { key: "key_file", secret: false, required: false },
    { key: "known_hosts_file", secret: false, required: false },
    { key: "pass", secret: true, required: false },
  ],
  webdav: [
    { key: "url", secret: false, required: true },
    { key: "user", secret: false, required: true },
    { key: "vendor", secret: false, required: false },
    { key: "pass", secret: true, required: true },
  ],
  rclone: [{ key: "remote", secret: false, required: true }],
};

const PATH_REQUIRED: Record<BackupDestinationType, boolean> = {
  local: true,
  smb: true,
  s3: true,
  sftp: false,
  webdav: false,
  rclone: false,
};

const DEFAULT_RETENTION = { daily: 7, weekly: 4, monthly: 6 };
const RETENTION_PERIODS = ["daily", "weekly", "monthly"] as const;

const NAME_ID = "backup-dest-name";
const PATH_ID = "backup-dest-path";
const PASSPHRASE_ID = "backup-passphrase";
const PASSPHRASE_CONFIRM_ID = "backup-passphrase-confirm";
const NEW_PASSWORD_AUTOCOMPLETE = "new-password";

function fieldId(key: string): string {
  return `backup-dest-${key}`;
}

function fieldHintKey(key: string): string {
  return `settings.backup.fields.${key}.hint`;
}

function retentionId(period: string): string {
  return `backup-dest-retention-${period}`;
}

type RequestOutcome<T> =
  | { ok: true; data: T | undefined }
  | { ok: false; code: string | undefined; message: string };

async function request<T>(
  call: () => Promise<ClientResult<T>>,
  fallback: string,
): Promise<RequestOutcome<T>> {
  try {
    const result = await call();
    const parsed = parseClientResult(result, fallback);
    if (parsed.error !== null) {
      return { ok: false, code: result.error?.code, message: parsed.error };
    }
    return { ok: true, data: parsed.data };
  } catch (err: unknown) {
    return { ok: false, code: undefined, message: err instanceof Error ? err.message : fallback };
  }
}

function formatDateTime(iso: string, locale: string): string {
  return new Intl.DateTimeFormat(locale, { dateStyle: "medium", timeStyle: "short" }).format(new Date(iso));
}

function saveBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
}

function configArchiveName(): string {
  const stamp = new Date().toISOString().slice(0, 19).replace(/:/g, "-");
  return `hoserva-config-${stamp}.tar.zst`;
}

function typeLabel(type: string, t: (key: string, options?: Record<string, string>) => string): string {
  return t(`settings.backup.destinations.types.${type}`, { defaultValue: type });
}

function destinationStatus(
  destination: BackupDestination,
  t: (key: string) => string,
): React.ReactElement {
  if (destination.stale) {
    return (
      <div className="flex flex-col gap-1">
        <StatusBadge tone="warning">{t("settings.backup.destinations.status.stale")}</StatusBadge>
        <span className="text-muted-foreground text-xs">{t("settings.backup.destinations.status.staleHint")}</span>
      </div>
    );
  }
  if (!destination.enabled) {
    return <StatusBadge tone="outline">{t("settings.backup.destinations.status.paused")}</StatusBadge>;
  }
  if (destination.lastSuccessfulBackupAt === undefined) {
    return <StatusBadge tone="info">{t("settings.backup.destinations.status.waiting")}</StatusBadge>;
  }
  return <StatusBadge tone="success">{t("settings.backup.destinations.status.ok")}</StatusBadge>;
}

type DestinationForm = {
  name: string;
  type: BackupDestinationType;
  path: string;
  enabled: boolean;
  encrypt: boolean;
  daily: number;
  weekly: number;
  monthly: number;
  values: Record<string, string>;
};

const emptyForm = (): DestinationForm => ({
  name: "",
  type: "local",
  path: "",
  enabled: true,
  encrypt: false,
  daily: DEFAULT_RETENTION.daily,
  weekly: DEFAULT_RETENTION.weekly,
  monthly: DEFAULT_RETENTION.monthly,
  values: {},
});

function fieldsFor(type: BackupDestinationType): FieldSpec[] {
  return type === "local" ? [] : REMOTE_FIELDS[type];
}

function formComplete(form: DestinationForm): boolean {
  if (form.name.trim() === "") {
    return false;
  }
  if (PATH_REQUIRED[form.type] && form.path.trim() === "") {
    return false;
  }
  return fieldsFor(form.type).every((field) => !field.required || (form.values[field.key] ?? "").trim() !== "");
}

function formToRequest(form: DestinationForm): CreateBackupDestinationRequest {
  const options: Record<string, string> = {};
  const secrets: Record<string, string> = {};
  for (const field of fieldsFor(form.type)) {
    const raw = form.values[field.key] ?? "";
    if (field.secret) {
      if (raw !== "") {
        secrets[field.key] = raw;
      }
    } else if (raw.trim() !== "") {
      options[field.key] = raw.trim();
    }
  }
  const body: CreateBackupDestinationRequest = {
    name: form.name.trim(),
    type: form.type,
    path: form.path.trim(),
    enabled: form.enabled,
    retention: { daily: form.daily, weekly: form.weekly, monthly: form.monthly },
  };
  if (form.type === "local") {
    body.encrypt = form.encrypt;
  }
  if (Object.keys(options).length > 0) {
    body.options = options;
  }
  if (Object.keys(secrets).length > 0) {
    body.secrets = secrets;
  }
  return body;
}

function AddDestinationOverlay({
  passphraseSet,
  onClose,
  onNeedPassphrase,
  onAdded,
}: {
  passphraseSet: boolean | null;
  onClose: () => void;
  onNeedPassphrase: () => void;
  onAdded: () => Promise<void>;
}): React.ReactElement {
  const { t, i18n } = useTranslation();
  const [form, setForm] = useState<DestinationForm>(emptyForm);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<{ code: string | undefined; message: string } | null>(null);

  const isRemote = form.type !== "local";

  async function handleSubmit(): Promise<void> {
    setBusy(true);
    setFailure(null);
    try {
      const outcome = await request<BackupDestination>(
        () => postBackupDestination(formToRequest(form)),
        t("settings.backup.addForm.failed"),
      );
      if (!outcome.ok) {
        setFailure({ code: outcome.code, message: outcome.message });
        return;
      }
      showFeedbackToast({
        type: "success",
        title: t("settings.backup.addForm.added"),
        description: form.name.trim(),
      });
      onClose();
      await onAdded();
    } finally {
      setBusy(false);
    }
  }

  function failureBanner(current: { code: string | undefined; message: string }): React.ReactElement {
    if (current.code === CODE_PASSPHRASE_REQUIRED) {
      return (
        <Banner
          tone="error"
          title={t("settings.backup.addForm.passphraseNeededTitle")}
          description={current.message}
          action={
            <Button size="sm" variant="outline" onClick={onNeedPassphrase}>
              {t("settings.backup.addForm.setPassphrase")}
            </Button>
          }
        />
      );
    }
    if (current.code === CODE_RCLONE_MISSING) {
      return (
        <Banner tone="error" title={t("settings.backup.destinations.rcloneMissing")} description={current.message} />
      );
    }
    return <Banner tone="error" title={t("settings.backup.addForm.failed")} description={current.message} />;
  }

  return (
    <FormOverlay
      open
      onOpenChange={(open) => {
        if (!open && !busy) {
          onClose();
        }
      }}
      title={t("settings.backup.addForm.title")}
      description={t("settings.backup.addForm.description")}
      footer={
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" disabled={busy} onClick={onClose}>
            {t("settings.actions.cancel")}
          </Button>
          <Button type="button" loading={busy} disabled={!formComplete(form)} onClick={() => void handleSubmit()}>
            {t("settings.backup.addForm.submit")}
          </Button>
        </div>
      }
    >
      {failure ? failureBanner(failure) : null}
      {isRemote && passphraseSet === false && failure?.code !== CODE_PASSPHRASE_REQUIRED ? (
        <Banner
          tone="warning"
          title={t("settings.backup.addForm.passphraseNeededTitle")}
          description={t("settings.backup.addForm.passphraseNeededDescription")}
          action={
            <Button size="sm" variant="outline" disabled={busy} onClick={onNeedPassphrase}>
              {t("settings.backup.addForm.setPassphrase")}
            </Button>
          }
        />
      ) : null}
      <Field>
        <FieldLabel htmlFor={NAME_ID}>{t("settings.backup.addForm.name")}</FieldLabel>
        <Input
          id={NAME_ID}
          value={form.name}
          onChange={(event) => setForm((current) => ({ ...current, name: event.target.value }))}
          placeholder={t("settings.backup.addForm.namePlaceholder")}
        />
      </Field>
      <Field>
        <FieldLabel>{t("settings.backup.addForm.type")}</FieldLabel>
        <Select
          value={form.type}
          onValueChange={(value) =>
            value && setForm((current) => ({ ...current, type: value as BackupDestinationType, values: {} }))
          }
        >
          <SelectTrigger aria-label={t("settings.backup.addForm.type")}>
            <SelectValue>{(value: string) => typeLabel(value, t)}</SelectValue>
          </SelectTrigger>
          <SelectPopup>
            {DESTINATION_TYPES.map((type) => (
              <SelectItem key={type} value={type}>
                {typeLabel(type, t)}
              </SelectItem>
            ))}
          </SelectPopup>
        </Select>
      </Field>
      <Field>
        <FieldLabel htmlFor={PATH_ID}>{t(`settings.backup.addForm.path.${form.type}`)}</FieldLabel>
        <Input
          id={PATH_ID}
          value={form.path}
          onChange={(event) => setForm((current) => ({ ...current, path: event.target.value }))}
        />
        <FieldDescription>{t(`settings.backup.addForm.pathHint.${form.type}`)}</FieldDescription>
      </Field>
      {fieldsFor(form.type).map((field) => {
        const id = fieldId(field.key);
        const hintKey = fieldHintKey(field.key);
        let hint = "";
        if (field.secret) {
          hint = t("settings.backup.addForm.secretHint");
        } else if (i18n.exists(hintKey)) {
          hint = t(hintKey);
        }
        return (
          <Field key={`${form.type}-${field.key}`}>
            <FieldLabel htmlFor={id}>{t(`settings.backup.fields.${field.key}.label`)}</FieldLabel>
            {field.secret ? (
              <SecretInput
                id={id}
                value={form.values[field.key] ?? ""}
                onChange={(value) =>
                  setForm((current) => ({ ...current, values: { ...current.values, [field.key]: value } }))
                }
                autoComplete={NEW_PASSWORD_AUTOCOMPLETE}
              />
            ) : (
              <Input
                id={id}
                value={form.values[field.key] ?? ""}
                onChange={(event) =>
                  setForm((current) => ({
                    ...current,
                    values: { ...current.values, [field.key]: event.target.value },
                  }))
                }
              />
            )}
            {hint !== "" ? <FieldDescription>{hint}</FieldDescription> : null}
          </Field>
        );
      })}
      <SettingSwitch
        label={t("settings.backup.addForm.enabled")}
        description={t("settings.backup.addForm.enabledHint")}
        checked={form.enabled}
        onCheckedChange={(enabled) => setForm((current) => ({ ...current, enabled }))}
      />
      {isRemote ? (
        <p className="text-muted-foreground text-sm">{t("settings.backup.addForm.encryptRemote")}</p>
      ) : (
        <SettingSwitch
          label={t("settings.backup.addForm.encrypt")}
          description={t("settings.backup.addForm.encryptHint")}
          checked={form.encrypt}
          onCheckedChange={(encrypt) => setForm((current) => ({ ...current, encrypt }))}
        />
      )}
      <fieldset className="flex flex-col gap-2">
        <legend className="font-medium text-sm">{t("settings.backup.addForm.retention")}</legend>
        <p className="text-muted-foreground text-sm">{t("settings.backup.addForm.retentionHint")}</p>
        <div className="grid gap-3 sm:grid-cols-3">
          {RETENTION_PERIODS.map((period) => (
            <Field key={period}>
              <FieldLabel htmlFor={retentionId(period)}>
                {t(`settings.backup.addForm.${period}`)}
              </FieldLabel>
              <NumberUnit
                id={retentionId(period)}
                value={form[period]}
                onChange={(value) => setForm((current) => ({ ...current, [period]: value }))}
                unit={t("settings.backup.addForm.archivesUnit")}
                min={0}
                max={1000}
              />
            </Field>
          ))}
        </div>
      </fieldset>
    </FormOverlay>
  );
}

function DestinationsSection({
  passphraseSet,
  onNeedPassphrase,
}: {
  passphraseSet: boolean | null;
  onNeedPassphrase: () => void;
}): React.ReactElement {
  const { t, i18n } = useTranslation();
  const query = useApiQuery<DestinationsLoad>({
    queryKey: "backup-destinations",
    queryFn: (signal) => getBackupDestinations(signal),
    fallbackError: t("settings.backup.destinations.loadFailed"),
  });
  const [addOpen, setAddOpen] = useState(false);
  const [testingIds, setTestingIds] = useState<string[]>([]);
  const [removeTarget, setRemoveTarget] = useState<BackupDestination | null>(null);
  const [removeBusy, setRemoveBusy] = useState(false);
  const [removeError, setRemoveError] = useState<string | null>(null);

  async function handleTest(destination: BackupDestination): Promise<void> {
    setTestingIds((current) => [...current, destination.id]);
    try {
      const outcome = await request<BackupDestinationTestResult>(
        () => postBackupDestinationTest(destination.id),
        t("settings.backup.destinations.testError", { name: destination.name }),
      );
      if (!outcome.ok) {
        showFeedbackToast({
          type: "error",
          title:
            outcome.code === CODE_RCLONE_MISSING
              ? t("settings.backup.destinations.rcloneMissing")
              : t("settings.backup.destinations.testError", { name: destination.name }),
          description: outcome.message,
        });
        return;
      }
      if (outcome.data?.success === true) {
        showFeedbackToast({
          type: "success",
          title: t("settings.backup.destinations.testPassed"),
          description: t("settings.backup.destinations.testPassedDescription", { name: destination.name }),
        });
        return;
      }
      showFeedbackToast({
        type: "error",
        title: t("settings.backup.destinations.testFailed"),
        description: outcome.data?.error || t("settings.backup.destinations.testFailedUnknown"),
      });
    } finally {
      setTestingIds((current) => current.filter((id) => id !== destination.id));
    }
  }

  async function handleRemove(): Promise<void> {
    if (!removeTarget) {
      return;
    }
    const target = removeTarget;
    setRemoveBusy(true);
    setRemoveError(null);
    try {
      const outcome = await request<unknown>(
        () => deleteBackupDestination(target.id),
        t("settings.backup.destinations.removeFailed"),
      );
      if (!outcome.ok) {
        setRemoveError(outcome.message);
        return;
      }
      setRemoveTarget(null);
      showFeedbackToast({
        type: "success",
        title: t("settings.backup.destinations.removed"),
        description: target.name,
      });
      await query.refresh();
    } finally {
      setRemoveBusy(false);
    }
  }

  const load = query.data;
  let body: React.ReactElement;
  if (query.loading) {
    body = <LoadingBlock />;
  } else if (load === null) {
    body = (
      <Banner
        tone="error"
        title={t("settings.backup.destinations.loadFailed")}
        description={query.error ?? undefined}
      />
    );
  } else if (!load.available) {
    body = (
      <Banner
        tone="info"
        title={t("settings.backup.unavailableTitle")}
        description={t("settings.backup.unavailableDescription")}
      />
    );
  } else if (load.value.destinations.length === 0) {
    body = (
      <EmptyState
        icon={Archive}
        title={t("settings.backup.destinations.empty.title")}
        description={t("settings.backup.destinations.empty.description")}
        action={<Button onClick={() => setAddOpen(true)}>{t("settings.backup.destinations.add")}</Button>}
      />
    );
  } else {
    const columns: DataTableColumn<BackupDestination>[] = [
      {
        id: "name",
        header: t("settings.backup.destinations.columns.name"),
        cell: (row) => (
          <div className="flex flex-col">
            <span className="font-medium">{row.name}</span>
            {row.path !== "" ? <span className="text-muted-foreground text-xs">{row.path}</span> : null}
          </div>
        ),
      },
      {
        id: "type",
        header: t("settings.backup.destinations.columns.type"),
        cell: (row) => typeLabel(row.type, t),
      },
      {
        id: "enabled",
        header: t("settings.backup.destinations.columns.enabled"),
        cell: (row) => (
          <StatusBadge tone={row.enabled ? "success" : "outline"}>
            {row.enabled ? t("settings.backup.destinations.enabled") : t("settings.backup.destinations.disabled")}
          </StatusBadge>
        ),
      },
      {
        id: "retention",
        header: t("settings.backup.destinations.columns.retention"),
        cell: (row) =>
          t("settings.backup.destinations.retention", {
            daily: row.retention.daily,
            weekly: row.retention.weekly,
            monthly: row.retention.monthly,
          }),
      },
      {
        id: "encryption",
        header: t("settings.backup.destinations.columns.encryption"),
        cell: (row) =>
          row.encrypt ? t("settings.backup.destinations.encrypted") : t("settings.backup.destinations.notEncrypted"),
      },
      {
        id: "lastBackup",
        header: t("settings.backup.destinations.columns.lastBackup"),
        cell: (row) =>
          row.lastSuccessfulBackupAt
            ? formatDateTime(row.lastSuccessfulBackupAt, i18n.language)
            : t("settings.backup.destinations.neverBackedUp"),
      },
      {
        id: "status",
        header: t("settings.backup.destinations.columns.status"),
        cell: (row) => destinationStatus(row, t),
      },
      {
        id: "actions",
        header: t("settings.backup.destinations.columns.actions"),
        cell: (row) => (
          <div className="flex flex-wrap gap-2">
            <Button
              size="sm"
              variant="outline"
              loading={testingIds.includes(row.id)}
              aria-label={t("settings.backup.destinations.testFor", { name: row.name })}
              onClick={() => void handleTest(row)}
            >
              {t("settings.backup.destinations.test")}
            </Button>
            <Button
              size="sm"
              variant="ghost"
              aria-label={t("settings.backup.destinations.removeFor", { name: row.name })}
              onClick={() => {
                setRemoveError(null);
                setRemoveTarget(row);
              }}
            >
              {t("settings.backup.destinations.remove")}
            </Button>
          </div>
        ),
      },
    ];
    body = <DataTable rows={load.value.destinations} getRowKey={(row) => row.id} columns={columns} />;
  }

  const canAdd = load !== null && load.available;

  return (
    <section className="flex flex-col gap-3">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <h2 className="font-medium text-lg">{t("settings.backup.destinations.title")}</h2>
          <p className="text-muted-foreground text-sm">{t("settings.backup.destinations.description")}</p>
          <p className="text-muted-foreground text-sm">{t("settings.backup.destinations.nfsNote")}</p>
        </div>
        <Button disabled={!canAdd} onClick={() => setAddOpen(true)}>
          {t("settings.backup.destinations.add")}
        </Button>
      </div>
      {query.error !== null && load !== null ? (
        <Banner tone="error" title={t("settings.backup.destinations.loadFailed")} description={query.error} />
      ) : null}
      {body}

      {addOpen ? (
        <AddDestinationOverlay
          passphraseSet={passphraseSet}
          onClose={() => setAddOpen(false)}
          onNeedPassphrase={() => {
            setAddOpen(false);
            onNeedPassphrase();
          }}
          onAdded={query.refresh}
        />
      ) : null}

      <ConfirmDialog
        open={removeTarget !== null}
        onOpenChange={(open) => {
          if (!open && !removeBusy) {
            setRemoveTarget(null);
          }
        }}
        title={t("settings.backup.destinations.removeTitle", { name: removeTarget?.name ?? "" })}
        description={t("settings.backup.destinations.removeDescription")}
        items={[t("settings.backup.destinations.removeKept", { name: removeTarget?.name ?? "" })]}
        error={removeError}
        confirmLabel={t("settings.backup.destinations.removeConfirm")}
        destructive
        loading={removeBusy}
        onConfirm={() => void handleRemove()}
      />
    </section>
  );
}

function PassphraseOverlay({
  passphraseSet,
  onClose,
  onSaved,
}: {
  passphraseSet: boolean;
  onClose: () => void;
  onSaved: () => Promise<void>;
}): React.ReactElement {
  const { t } = useTranslation();
  const [passphrase, setPassphrase] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const mismatch = confirm !== "" && confirm !== passphrase;

  async function handleSave(): Promise<void> {
    setBusy(true);
    setError(null);
    try {
      const outcome = await request<GeneralSettings>(
        () => putGeneralSettings({ backupPassphrase: passphrase }),
        t("settings.backup.passphrase.saveFailed"),
      );
      if (!outcome.ok) {
        setError(outcome.message);
        return;
      }
      showFeedbackToast({ type: "success", title: t("settings.backup.passphrase.saved") });
      onClose();
      await onSaved();
    } finally {
      setBusy(false);
    }
  }

  return (
    <FormOverlay
      open
      onOpenChange={(open) => {
        if (!open && !busy) {
          onClose();
        }
      }}
      title={passphraseSet ? t("settings.backup.passphrase.changeTitle") : t("settings.backup.passphrase.setTitle")}
      description={t("settings.backup.passphrase.description")}
      footer={
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" disabled={busy} onClick={onClose}>
            {t("settings.actions.cancel")}
          </Button>
          <Button
            type="button"
            loading={busy}
            disabled={passphrase === "" || confirm !== passphrase}
            onClick={() => void handleSave()}
          >
            {t("settings.backup.passphrase.save")}
          </Button>
        </div>
      }
    >
      {error ? <Banner tone="error" title={t("settings.backup.passphrase.saveFailed")} description={error} /> : null}
      <Banner
        tone="warning"
        title={t("settings.backup.passphrase.warning")}
        description={passphraseSet ? t("settings.backup.passphrase.changeWarning") : undefined}
      />
      <Field>
        <FieldLabel htmlFor={PASSPHRASE_ID}>{t("settings.backup.passphrase.passphrase")}</FieldLabel>
        <SecretInput
          id={PASSPHRASE_ID}
          value={passphrase}
          onChange={setPassphrase}
          showStrength
          showGenerate
          autoComplete={NEW_PASSWORD_AUTOCOMPLETE}
        />
      </Field>
      <Field>
        <FieldLabel htmlFor={PASSPHRASE_CONFIRM_ID}>{t("settings.backup.passphrase.confirm")}</FieldLabel>
        <SecretInput
          id={PASSPHRASE_CONFIRM_ID}
          value={confirm}
          onChange={setConfirm}
          aria-invalid={mismatch}
          autoComplete={NEW_PASSWORD_AUTOCOMPLETE}
        />
        {mismatch ? <FieldDescription>{t("settings.backup.passphrase.mismatch")}</FieldDescription> : null}
      </Field>
    </FormOverlay>
  );
}

function ConfigBackupCard({
  general,
  onEditPassphrase,
}: {
  general: UseApiQueryResult<GeneralSettings>;
  onEditPassphrase: () => void;
}): React.ReactElement {
  const { t } = useTranslation();
  const [downloadBusy, setDownloadBusy] = useState(false);
  const [downloadError, setDownloadError] = useState<string | null>(null);

  async function handleDownload(): Promise<void> {
    setDownloadBusy(true);
    setDownloadError(null);
    try {
      const outcome = await request<Blob>(() => postConfigExport(), t("settings.backup.config.downloadFailed"));
      if (!outcome.ok) {
        setDownloadError(outcome.message);
        return;
      }
      if (outcome.data === undefined) {
        setDownloadError(t("settings.backup.config.downloadFailed"));
        return;
      }
      saveBlob(outcome.data, configArchiveName());
      showFeedbackToast({ type: "success", title: t("settings.backup.config.downloaded") });
    } finally {
      setDownloadBusy(false);
    }
  }

  const passphraseSet = general.data?.backupPassphraseSet;

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("settings.backup.config.title")}</CardTitle>
        <CardDescription>{t("settings.backup.config.description")}</CardDescription>
      </CardHeader>
      <CardPanel className="flex flex-col gap-4">
        {downloadError ? (
          <Banner tone="error" title={t("settings.backup.config.downloadFailed")} description={downloadError} />
        ) : null}
        <div className="flex flex-col gap-2">
          <div>
            <Button loading={downloadBusy} onClick={() => void handleDownload()}>
              {t("settings.backup.config.download")}
            </Button>
          </div>
          <p className="text-muted-foreground text-sm">{t("settings.backup.config.downloadOnly")}</p>
        </div>
        <div className="flex flex-col gap-2 border-t pt-4">
          <p className="font-medium text-sm">{t("settings.backup.config.passphraseTitle")}</p>
          {general.loading ? <LoadingBlock rows={1} /> : null}
          {general.error !== null ? (
            <Banner
              tone="error"
              title={t("settings.backup.config.passphraseLoadFailed")}
              description={general.error}
            />
          ) : null}
          {passphraseSet !== undefined ? (
            <div className="flex flex-wrap items-center gap-3">
              <StatusBadge tone={passphraseSet ? "success" : "warning"}>
                {passphraseSet
                  ? t("settings.backup.config.passphraseSet")
                  : t("settings.backup.config.passphraseNotSet")}
              </StatusBadge>
              <Button size="sm" variant="outline" onClick={onEditPassphrase}>
                {passphraseSet
                  ? t("settings.backup.config.changePassphrase")
                  : t("settings.backup.config.setPassphrase")}
              </Button>
            </div>
          ) : null}
          <p className="text-muted-foreground text-sm">{t("settings.backup.config.passphraseHint")}</p>
        </div>
      </CardPanel>
    </Card>
  );
}

function RestoreDrillCard(): React.ReactElement {
  const { t, i18n } = useTranslation();
  const drillQuery = useApiQuery<Availability<RestoreDrill>>({
    queryKey: "backup-restore-drill",
    queryFn: (signal) => getRestoreDrill(signal),
    fallbackError: t("settings.backup.drill.loadFailed"),
  });
  const schedulesQuery = useApiQuery<Schedules>({
    queryKey: "backup-drill-schedule",
    queryFn: (signal) => getSchedules(signal),
    fallbackError: t("settings.backup.drill.scheduleUnavailable"),
  });
  const [runBusy, setRunBusy] = useState(false);
  const [runError, setRunError] = useState<string | null>(null);
  const [queuedJob, setQueuedJob] = useState<Job | null>(null);

  async function handleRun(): Promise<void> {
    setRunBusy(true);
    setRunError(null);
    try {
      const outcome = await request<Job>(() => postRestoreDrill(), t("settings.backup.drill.runFailed"));
      if (!outcome.ok) {
        setRunError(outcome.message);
        return;
      }
      setQueuedJob(outcome.data ?? null);
    } finally {
      setRunBusy(false);
    }
  }

  const drill = drillQuery.data;
  const lastRun = drill !== null && drill.available ? drill.value.lastRun : undefined;

  let result: React.ReactElement;
  if (drillQuery.loading) {
    result = <LoadingBlock rows={2} />;
  } else if (drill === null) {
    result = (
      <Banner tone="error" title={t("settings.backup.drill.loadFailed")} description={drillQuery.error ?? undefined} />
    );
  } else if (!drill.available) {
    result = (
      <Banner
        tone="info"
        title={t("settings.backup.unavailableTitle")}
        description={t("settings.backup.unavailableDescription")}
      />
    );
  } else if (lastRun === undefined) {
    result = (
      <div className="flex flex-col gap-1">
        <div>
          <StatusBadge tone="outline">{t("settings.backup.drill.neverRunTitle")}</StatusBadge>
        </div>
        <p className="text-muted-foreground text-sm">{t("settings.backup.drill.neverRunDescription")}</p>
      </div>
    );
  } else {
    result = (
      <div className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center gap-3">
          <StatusBadge tone={lastRun.passed ? "success" : "error"}>
            {lastRun.passed ? t("settings.backup.drill.passed") : t("settings.backup.drill.failed")}
          </StatusBadge>
          <span className="text-muted-foreground text-sm">
            {t("settings.backup.drill.ranAt", { when: formatDateTime(lastRun.ranAt, i18n.language) })}
          </span>
        </div>
        {lastRun.error ? (
          <Banner tone="error" title={t("settings.backup.drill.noDestinationsTested")} description={lastRun.error} />
        ) : null}
        {lastRun.destinations.length > 0 ? (
          <div className="flex flex-col gap-2">
            <p className="font-medium text-sm">{t("settings.backup.drill.destinationsTitle")}</p>
            <ul className="flex flex-col gap-2">
              {lastRun.destinations.map((entry) => (
                <li key={entry.destinationId} className="flex flex-col gap-1 rounded-lg border p-3 text-sm">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="font-medium">{entry.destinationName}</span>
                    <StatusBadge tone={entry.passed ? "success" : "error"}>
                      {entry.passed ? t("settings.backup.drill.passed") : t("settings.backup.drill.failed")}
                    </StatusBadge>
                  </div>
                  <span className="text-muted-foreground">
                    {entry.archive
                      ? t("settings.backup.drill.archiveTested", { archive: entry.archive })
                      : t("settings.backup.drill.noArchive")}
                  </span>
                  {!entry.passed ? (
                    <span className="text-destructive-foreground">
                      {entry.error || t("settings.backup.drill.reasonUnknown")}
                    </span>
                  ) : null}
                </li>
              ))}
            </ul>
          </div>
        ) : null}
      </div>
    );
  }

  let nextRun: string;
  if (schedulesQuery.loading) {
    nextRun = t("loading.label");
  } else if (schedulesQuery.data === null) {
    nextRun = t("settings.backup.drill.scheduleUnavailable");
  } else {
    const job = schedulesQuery.data.otherJobs.find((entry) => entry.id === "restore_drill");
    if (job === undefined) {
      nextRun = t("settings.backup.drill.scheduleUnavailable");
    } else if (!job.enabled) {
      nextRun = t("settings.backup.drill.notScheduled");
    } else {
      nextRun = formatDateTime(job.nextRun, i18n.language);
    }
  }

  const unavailable = drill !== null && !drill.available;

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("settings.backup.drill.title")}</CardTitle>
        <CardDescription>{t("settings.backup.drill.description")}</CardDescription>
        <CardAction>
          <div className="flex gap-2">
            <Button
              variant="outline"
              disabled={drillQuery.refreshing}
              onClick={() => void drillQuery.refresh()}
            >
              {t("settings.backup.drill.refresh")}
            </Button>
            <Button loading={runBusy} disabled={unavailable} onClick={() => void handleRun()}>
              {t("settings.backup.drill.runNow")}
            </Button>
          </div>
        </CardAction>
      </CardHeader>
      <CardPanel className="flex flex-col gap-4">
        {runError ? <Banner tone="error" title={t("settings.backup.drill.runFailed")} description={runError} /> : null}
        {queuedJob ? (
          <Banner
            tone="info"
            title={t("settings.backup.drill.queuedTitle")}
            description={t("settings.backup.drill.queuedDescription", { status: jobStatusLabel(queuedJob.status, t) })}
            action={
              <Button size="sm" variant="outline" render={<Link to={jobDetailPath(queuedJob.id)} />}>
                {t("settings.backup.drill.viewJob")}
              </Button>
            }
          />
        ) : null}
        {drillQuery.error !== null && drill !== null ? (
          <Banner tone="error" title={t("settings.backup.drill.loadFailed")} description={drillQuery.error} />
        ) : null}
        {result}
        <div>
          <p className="font-medium text-sm">{t("settings.backup.drill.nextRun")}</p>
          <p className="text-muted-foreground text-sm">{nextRun}</p>
        </div>
      </CardPanel>
    </Card>
  );
}

export function BackupSettingsPage(): React.ReactElement {
  const { t } = useTranslation();
  const generalQuery = useApiQuery<GeneralSettings>({
    queryKey: "backup-general-settings",
    queryFn: (signal) => getGeneralSettings(signal),
    fallbackError: t("settings.backup.config.passphraseLoadFailed"),
  });
  const [passphraseOpen, setPassphraseOpen] = useState(false);

  const passphraseSet = generalQuery.data?.backupPassphraseSet ?? null;

  return (
    <div className="flex flex-col gap-6">
      <DestinationsSection passphraseSet={passphraseSet} onNeedPassphrase={() => setPassphraseOpen(true)} />
      <ConfigBackupCard general={generalQuery} onEditPassphrase={() => setPassphraseOpen(true)} />
      <RestoreDrillCard />
      {passphraseOpen ? (
        <PassphraseOverlay
          passphraseSet={passphraseSet === true}
          onClose={() => setPassphraseOpen(false)}
          onSaved={generalQuery.refresh}
        />
      ) : null}
    </div>
  );
}
