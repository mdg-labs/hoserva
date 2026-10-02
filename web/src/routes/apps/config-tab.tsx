// The Config tab (doc 03 §5.5): the settings form of an app that was installed
// from a template, with the values the app has now. Everything about a value is
// decided by the API: `updateStackConfig` checks it with the rules the install
// form uses and writes only the stack's .env, and the page applies it by
// starting the stack again.
import { Boxes } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { ConfirmDialog } from "@/components/patterns/confirm";
import { EmptyState } from "@/components/patterns/empty-state";
import { InlineNote } from "@/components/patterns/inline-note";
import { LoadingBlock } from "@/components/patterns/loading";
import { SecretInput } from "@/components/patterns/secret-input";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Button, buttonVariants } from "@/components/ui/button";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import type { components } from "@/lib/api/client";
import { getStackConfig, startStack, updateStackConfig } from "@/lib/api/operations";
import { isAbortError, parseClientResult, type ClientResult } from "@/lib/api/request";
import { useApiQuery } from "@/lib/api/use-api-query";
import { JobFollow } from "@/routes/apps/compose";
import {
  asTemplateInput,
  buildConfigRequest,
  GENERATE,
  KEEP,
  NO_EDITS,
  REPLACE,
  secretMode,
  type ConfigEdits,
  type SecretMode,
  type StackConfig,
  type StackConfigInput,
} from "@/routes/apps/config-form";
import { appComposePath, type App } from "@/routes/apps/containers";
import { fieldOfError } from "@/routes/apps/install-form";
import { InputField } from "@/routes/apps/install";

type Job = components["schemas"]["Job"];

const NO_TEMPLATE_CODE = "stack_has_no_template";
const NOT_FOUND_CODE = "stack_not_found";
const APPS_PATH = "/apps";
const OFF = "off";
const NEW_PASSWORD = "new-password";

type Loaded = { kind: "config"; config: StackConfig } | { kind: "noTemplate" } | { kind: "notFound" };
type Failure = { message: string };
type Start = { kind: "queued"; job: Job } | { kind: "notStarted"; message: string };
type Phase = "idle" | "saving" | "starting" | "reloading";
type Unknown = { message: string | null; reload: "loading" | "done" | "failed" };

async function loadConfig(stack: string, signal: AbortSignal): Promise<ClientResult<Loaded>> {
  const result = await getStackConfig(stack, signal);
  if (result.error?.code === NO_TEMPLATE_CODE) {
    return { data: { kind: "noTemplate" }, response: { ok: true } };
  }
  if (result.error?.code === NOT_FOUND_CODE) {
    return { data: { kind: "notFound" }, response: { ok: true } };
  }
  if (result.error !== undefined || result.response?.ok === false || result.data === undefined) {
    return { error: result.error, response: { ok: false } };
  }
  return { data: { kind: "config", config: result.data }, response: { ok: true } };
}

function NotFoundState({ name }: { name: string }): React.ReactElement {
  const { t } = useTranslation();
  return (
    <EmptyState
      icon={Boxes}
      title={t("apps.config.notFound.title")}
      description={t("apps.config.notFound.description", { name })}
      action={
        <Link to={APPS_PATH} className={buttonVariants()}>
          {t("apps.detail.notFound.action")}
        </Link>
      }
    />
  );
}

function NoTemplateNote({ app }: { app: App }): React.ReactElement {
  const { t } = useTranslation();
  return (
    <div className="flex flex-col items-start gap-3">
      <InlineNote
        title={t("apps.config.noTemplate.title")}
        description={
          app.stack === undefined
            ? t("apps.config.noTemplate.unmanaged", { name: app.name })
            : t("apps.config.noTemplate.noForm", { name: app.name })
        }
      />
      <Link to={appComposePath(app.name)} className={buttonVariants({ variant: "outline" })}>
        {t("apps.config.noTemplate.editor")}
      </Link>
    </div>
  );
}

function fieldId(name: string): string {
  return `config-${name}`;
}

function SecretField({
  input,
  mode,
  value,
  error,
  disabled,
  onMode,
  onValue,
}: {
  input: StackConfigInput;
  mode: SecretMode;
  value: string;
  error: string | null;
  disabled: boolean;
  onMode: (mode: SecretMode) => void;
  onValue: (value: string) => void;
}): React.ReactElement {
  const { t } = useTranslation();
  const id = fieldId(input.name);
  return (
    <Field invalid={error !== null} className="w-full">
      <FieldLabel htmlFor={id}>{input.label ?? input.name}</FieldLabel>
      <p className="text-sm">{input.set ? t("apps.config.secret.set") : t("apps.config.secret.notSet")}</p>
      {mode === REPLACE ? (
        <SecretInput
          id={id}
          value={value}
          onChange={onValue}
          showStrength
          autoComplete={NEW_PASSWORD}
          placeholder={t("apps.config.secret.placeholder")}
          aria-invalid={error !== null}
        />
      ) : null}
      {mode === GENERATE ? (
        <p className="text-muted-foreground text-sm">{t("apps.config.secret.willGenerate")}</p>
      ) : null}
      <div className="flex flex-wrap gap-2">
        <Button
          type="button"
          size="xs"
          variant="outline"
          disabled={disabled}
          aria-pressed={mode === REPLACE}
          onClick={() => onMode(mode === REPLACE ? KEEP : REPLACE)}
        >
          {mode === REPLACE ? t("apps.config.secret.keep") : t("apps.config.secret.replace")}
        </Button>
        <Button
          type="button"
          size="xs"
          variant="outline"
          disabled={disabled}
          aria-pressed={mode === GENERATE}
          onClick={() => onMode(mode === GENERATE ? KEEP : GENERATE)}
        >
          {mode === GENERATE ? t("apps.config.secret.keep") : t("apps.config.secret.generate")}
        </Button>
      </div>
      {input.description ? <FieldDescription>{input.description}</FieldDescription> : null}
      {error !== null ? (
        <FieldError match role="alert">
          {error}
        </FieldError>
      ) : null}
    </Field>
  );
}

function DeviceField({ input }: { input: StackConfigInput }): React.ReactElement {
  const { t } = useTranslation();
  const id = fieldId(input.name);
  return (
    <Field className="w-full">
      <FieldLabel htmlFor={id}>{input.label ?? input.name}</FieldLabel>
      <Input id={id} value={input.value ?? ""} placeholder={t("apps.config.device.none")} readOnly disabled autoComplete={OFF} />
      <FieldDescription>{t("apps.config.device.help")}</FieldDescription>
    </Field>
  );
}

function ConfigForm({ app, stack, loaded }: { app: App; stack: string; loaded: StackConfig }): React.ReactElement {
  const { t } = useTranslation();
  const [config, setConfig] = useState(loaded);
  const [edits, setEdits] = useState<ConfigEdits>(NO_EDITS);
  const [phase, setPhase] = useState<Phase>("idle");
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [failure, setFailure] = useState<Failure | null>(null);
  const [start, setStart] = useState<Start | null>(null);
  const [unknown, setUnknown] = useState<Unknown | null>(null);
  const alive = useRef(true);

  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);

  const body = useMemo(() => buildConfigRequest(config.inputs, edits), [config.inputs, edits]);
  const busy = phase !== "idle";
  const touched = Object.keys(edits.values).length > 0 || Object.values(edits.secrets).some((mode) => mode !== KEEP);
  const inputNames = config.inputs.map((input) => input.name);
  const failedField = failure === null ? null : fieldOfError(failure.message, inputNames);

  function edit(name: string, value: string): void {
    setEdits((prev) => ({ ...prev, values: { ...prev.values, [name]: value } }));
    setFailure(null);
  }

  function setMode(name: string, mode: SecretMode): void {
    setEdits((prev) => {
      const values = { ...prev.values };
      if (mode !== REPLACE) {
        delete values[name];
      }
      return { values, secrets: { ...prev.secrets, [name]: mode } };
    });
    setFailure(null);
  }

  async function handleStart(): Promise<void> {
    setPhase("starting");
    try {
      const result = await startStack(stack);
      const parsed = parseClientResult(result, t("apps.config.startFailed"));
      if (parsed.error !== null || parsed.data === undefined) {
        setStart({ kind: "notStarted", message: parsed.error ?? t("apps.config.startFailed") });
      } else {
        setStart({ kind: "queued", job: parsed.data });
      }
    } catch (err: unknown) {
      if (!isAbortError(err)) {
        setStart({ kind: "notStarted", message: err instanceof Error ? err.message : t("apps.config.startFailed") });
      }
    } finally {
      setPhase("idle");
    }
  }

  async function reload(message: string | null): Promise<void> {
    setPhase("reloading");
    setUnknown({ message, reload: "loading" });
    let outcome: Unknown["reload"] = "failed";
    try {
      const parsed = parseClientResult(await getStackConfig(stack), t("apps.config.loadFailed"));
      if (parsed.error === null && parsed.data !== undefined) {
        setConfig(parsed.data);
        setEdits(NO_EDITS);
        outcome = "done";
      }
    } catch {
      outcome = "failed";
    }
    if (alive.current) {
      setUnknown({ message, reload: outcome });
      setPhase("idle");
    }
  }

  async function handleApply(): Promise<void> {
    if (body === null || busy) {
      return;
    }
    setFailure(null);
    setStart(null);
    setUnknown(null);
    setPhase("saving");
    let saved: StackConfig | undefined;
    let unanswered: string | null | undefined;
    try {
      const result = await updateStackConfig(stack, body);
      const parsed = parseClientResult(result, t("apps.config.saveFailed"));
      if (parsed.error !== null) {
        setFailure({ message: parsed.error });
      } else if (parsed.data === undefined) {
        unanswered = null;
      } else {
        saved = parsed.data;
      }
    } catch (err: unknown) {
      unanswered = isAbortError(err) || !(err instanceof Error) ? null : err.message;
    }
    if (saved === undefined) {
      setConfirmOpen(false);
      if (unanswered !== undefined && alive.current) {
        await reload(unanswered);
      } else {
        setPhase("idle");
      }
      return;
    }
    setConfig(saved);
    setEdits(NO_EDITS);
    await handleStart();
    setConfirmOpen(false);
  }

  const startedJob = start?.kind === "queued" ? start.job : null;

  return (
    <div className="flex flex-col gap-4">
      {config.stack.manuallyEdited ? (
        <div className="flex flex-col items-start gap-2">
          <StatusBadge tone="warning">{t("apps.config.manuallyEdited")}</StatusBadge>
          <p className="text-muted-foreground text-sm">{t("apps.config.manuallyEditedHelp")}</p>
        </div>
      ) : null}

      {failure !== null && failedField === null ? (
        <Banner tone="error" title={t("apps.config.saveFailed")} description={failure.message} />
      ) : null}
      {unknown !== null ? (
        <Banner
          tone="warning"
          title={t("apps.config.saveUnknown.title")}
          description={
            <>
              <span className="block">{t(`apps.config.saveUnknown.${unknown.reload}`)}</span>
              {unknown.message !== null ? <span className="block break-words">{unknown.message}</span> : null}
            </>
          }
          action={
            unknown.reload === "failed" ? (
              <Button size="xs" variant="outline" onClick={() => void reload(unknown.message)}>
                {t("apps.installed.retry")}
              </Button>
            ) : undefined
          }
        />
      ) : null}
      {start?.kind === "notStarted" ? (
        <Banner
          tone="warning"
          title={t("apps.config.notStarted")}
          description={
            <>
              <span className="block">{t("apps.config.notStartedDescription")}</span>
              <span className="block break-words">{start.message}</span>
            </>
          }
          action={
            <Button size="xs" variant="outline" loading={phase === "starting"} onClick={() => void handleStart()}>
              {t("apps.config.retryStart")}
            </Button>
          }
        />
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle>{t("apps.config.title")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-4">
          <p className="text-muted-foreground text-sm">{t("apps.config.description")}</p>
          {config.inputs.length === 0 ? (
            <p className="text-muted-foreground text-sm">{t("apps.config.none")}</p>
          ) : (
            config.inputs.map((input) => {
              const error = failure !== null && failedField === input.name ? failure.message : null;
              if (input.kind === "device") {
                return <DeviceField key={input.name} input={input} />;
              }
              if (input.kind === "secret") {
                return (
                  <SecretField
                    key={input.name}
                    input={input}
                    mode={secretMode(edits, input.name)}
                    value={edits.values[input.name] ?? ""}
                    error={error}
                    disabled={busy}
                    onMode={(mode) => setMode(input.name, mode)}
                    onValue={(value) => edit(input.name, value)}
                  />
                );
              }
              return (
                <InputField
                  key={input.name}
                  input={asTemplateInput(input)}
                  value={edits.values[input.name] ?? input.value ?? ""}
                  optional={false}
                  error={error}
                  onChange={(value) => edit(input.name, value)}
                />
              );
            })
          )}
          <div className="flex flex-wrap items-center gap-2">
            <Button disabled={body === null || busy} onClick={() => setConfirmOpen(true)}>
              {t("apps.config.apply")}
            </Button>
            <Button variant="outline" disabled={!touched || busy} onClick={() => setEdits(NO_EDITS)}>
              {t("apps.config.discard")}
            </Button>
            {body !== null ? <p className="text-muted-foreground text-sm">{t("apps.config.unapplied")}</p> : null}
          </div>
        </CardPanel>
      </Card>

      {startedJob !== null ? <JobFollow queued={startedJob} stackName={stack} /> : null}

      <ConfirmDialog
        open={confirmOpen}
        onOpenChange={(next) => {
          if (!busy) {
            setConfirmOpen(next);
          }
        }}
        title={t("apps.config.confirm.title", { name: app.name })}
        description={t("apps.config.confirm.description")}
        confirmLabel={t("apps.config.apply")}
        loading={busy}
        onConfirm={() => void handleApply()}
      />
    </div>
  );
}

export function ConfigTab({ app }: { app: App }): React.ReactElement {
  const { t } = useTranslation();
  const stack = app.stack;
  const query = useApiQuery<Loaded>({
    queryKey: ["stack-config", stack],
    queryFn: (signal) => loadConfig(stack ?? "", signal),
    enabled: stack !== undefined,
    fallbackError: t("apps.config.loadFailed"),
  });

  if (stack === undefined) {
    return <NoTemplateNote app={app} />;
  }
  if (query.error) {
    return (
      <Banner
        tone="error"
        title={t("apps.config.loadFailed")}
        description={query.error}
        action={
          <Button size="xs" variant="outline" onClick={() => void query.refresh()}>
            {t("apps.installed.retry")}
          </Button>
        }
      />
    );
  }
  if (query.data === null) {
    return <LoadingBlock rows={3} />;
  }
  if (query.data.kind === "notFound") {
    return <NotFoundState name={app.name} />;
  }
  if (query.data.kind === "noTemplate") {
    return <NoTemplateNote app={app} />;
  }
  return <ConfigForm key={stack} app={app} stack={stack} loaded={query.data.config} />;
}
