import { ArrowLeft, FileX } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useBlocker, useParams } from "react-router-dom";

import { CodeEditor } from "@/components/code-editor";
import { Banner } from "@/components/patterns/banner";
import { ConfirmDialog } from "@/components/patterns/confirm";
import { EmptyState } from "@/components/patterns/empty-state";
import { FileUpload } from "@/components/patterns/file-upload";
import { FormOverlay } from "@/components/patterns/form-overlay";
import { JobProgress } from "@/components/patterns/job-progress";
import { LoadingBlock } from "@/components/patterns/loading";
import { StatusBadge } from "@/components/patterns/status-badge";
import { UnsavedGuardDialog } from "@/components/patterns/unsaved-guard";
import { Button, buttonVariants } from "@/components/ui/button";
import { Frame, FramePanel } from "@/components/ui/frame";
import { Textarea } from "@/components/ui/textarea";
import { Toolbar, ToolbarButton } from "@/components/ui/toolbar";
import { jobDetailPath } from "@/hooks/paths";
import type { components } from "@/lib/api/client";
import { getApp, getJob, getStack, startStack, updateStack } from "@/lib/api/operations";
import { apiErrorMessage, isAbortError, parseClientResult, type ClientResult } from "@/lib/api/request";
import { useApiQuery } from "@/lib/api/use-api-query";
import { appDetailPath } from "@/routes/apps/containers";

type Stack = components["schemas"]["Stack"];
type Job = components["schemas"]["Job"];

// The daemon refuses any request body over 64 KiB, JSON escaping included;
// 32 KiB of Compose text stays well under it.
export const MAX_IMPORT_BYTES = 32 * 1024;

const IMPORT_ACCEPT = ".yml,.yaml,text/yaml,application/yaml";
const INVALID_STACK_CODE = "invalid_stack";
const STACK_NOT_FOUND_CODE = "stack_not_found";
const INVALID_STACK_NAME_CODE = "invalid_stack_name";
const APP_NOT_FOUND_CODE = "app_not_found";
const DOCKER_UNAVAILABLE_CODE = "docker_unavailable";
const JOB_POLL_MS = 2000;
const FINISHED: Job["status"][] = ["succeeded", "failed", "cancelled", "interrupted"];

type Loaded = { kind: "stack"; stack: Stack; compose: string } | { kind: "notStack" };

// `:name` is the name in the app's URL, which is a container's name; a stack
// holds one or more containers under a name of its own. A container that
// names its stack is followed to it. A name no container has is tried as a
// stack's name, since a stack with nothing running has no container to
// ask. When Docker cannot be asked, a missing stack is not an answer.
async function loadStack(name: string, fallback: string, signal: AbortSignal): Promise<ClientResult<Loaded>> {
  const app = await getApp(name, signal);
  let stackName = name;
  let dockerError: string | null = null;
  if (app.error?.code === APP_NOT_FOUND_CODE) {
    stackName = name;
  } else if (app.error?.code === DOCKER_UNAVAILABLE_CODE) {
    dockerError = apiErrorMessage(app.error);
  } else if (app.error !== undefined || app.response?.ok === false || app.data === undefined) {
    return { error: app.error, response: { ok: false } };
  } else if (app.data.stack === undefined) {
    return { data: { kind: "notStack" }, response: { ok: true } };
  } else {
    stackName = app.data.stack;
  }

  const result = await getStack(stackName, signal);
  if (result.error?.code === STACK_NOT_FOUND_CODE || result.error?.code === INVALID_STACK_NAME_CODE) {
    if (dockerError !== null) {
      return { error: { code: DOCKER_UNAVAILABLE_CODE, message: dockerError }, response: { ok: false } };
    }
    return { data: { kind: "notStack" }, response: { ok: true } };
  }
  if (result.error !== undefined || result.response?.ok === false) {
    return { error: result.error, response: { ok: false } };
  }
  if (result.data === undefined || result.data.compose === undefined) {
    return { error: { code: "invalid_response", message: fallback }, response: { ok: false } };
  }
  return { data: { kind: "stack", stack: result.data, compose: result.data.compose }, response: { ok: true } };
}

type Outcome<T> =
  | { ok: true; data: T | undefined }
  | { ok: false; aborted: true }
  | { ok: false; aborted: false; code: string | undefined; message: string };

// The generated client returns { error } for an HTTP failure and rejects for
// a network one; both come out as a failed outcome, with the error code the
// page tells apart.
async function call<T>(run: () => Promise<ClientResult<T>>, fallback: string): Promise<Outcome<T>> {
  try {
    const result = await run();
    const parsed = parseClientResult(result, fallback);
    if (parsed.error !== null) {
      return { ok: false, aborted: false, code: result.error?.code, message: parsed.error };
    }
    return { ok: true, data: parsed.data };
  } catch (err: unknown) {
    if (isAbortError(err)) {
      return { ok: false, aborted: true };
    }
    return { ok: false, aborted: false, code: undefined, message: err instanceof Error ? err.message : fallback };
  }
}

function readText(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(typeof reader.result === "string" ? reader.result : "");
    reader.onerror = () => reject(reader.error ?? new Error());
    reader.readAsText(file);
  });
}

function byteLength(text: string): number {
  return new TextEncoder().encode(text).length;
}

function ImportDialog({
  onClose,
  onImport,
}: {
  onClose: () => void;
  onImport: (text: string) => void;
}): React.ReactElement {
  const { t } = useTranslation();
  const [text, setText] = useState("");
  const [problem, setProblem] = useState<string | null>(null);
  const [reading, setReading] = useState(false);

  const tooLarge = byteLength(text) > MAX_IMPORT_BYTES;

  async function handleFile(file: File | null): Promise<void> {
    setProblem(null);
    if (file === null) {
      return;
    }
    if (file.size > MAX_IMPORT_BYTES) {
      setProblem(t("apps.compose.import.tooLarge", { size: MAX_IMPORT_BYTES / 1024 }));
      return;
    }
    setReading(true);
    try {
      setText(await readText(file));
    } catch {
      setProblem(t("apps.compose.import.readFailed"));
    } finally {
      setReading(false);
    }
  }

  return (
    <FormOverlay
      open
      onOpenChange={(next) => {
        if (!next) {
          onClose();
        }
      }}
      title={t("apps.compose.import.title")}
      description={t("apps.compose.import.description")}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            {t("confirm.cancel")}
          </Button>
          <Button
            disabled={reading || tooLarge || text.trim() === ""}
            onClick={() => {
              onImport(text);
              onClose();
            }}
          >
            {t("apps.compose.import.confirm")}
          </Button>
        </>
      }
    >
      {problem !== null ? <Banner tone="error" title={problem} /> : null}
      <FileUpload
        id="compose-import-file"
        label={t("apps.compose.import.file")}
        description={t("apps.compose.import.fileHelp", { size: MAX_IMPORT_BYTES / 1024 })}
        accept={IMPORT_ACCEPT}
        disabled={reading}
        onChange={(file) => void handleFile(file)}
      />
      <div className="flex flex-col gap-2">
        <label htmlFor="compose-import-text" className="text-sm font-medium">
          {t("apps.compose.import.paste")}
        </label>
        <Textarea
          id="compose-import-text"
          className="font-mono"
          rows={8}
          spellCheck={false}
          value={text}
          onChange={(event) => setText(event.target.value)}
        />
        {tooLarge ? (
          <p role="alert" className="text-destructive-foreground text-sm">
            {t("apps.compose.import.tooLarge", { size: MAX_IMPORT_BYTES / 1024 })}
          </p>
        ) : null}
      </div>
    </FormOverlay>
  );
}

type Status =
  | { kind: "checked" }
  | { kind: "checkFailed"; code?: string; message: string }
  | { kind: "saveFailed"; code?: string; message: string }
  | { kind: "notStarted"; message: string }
  | { kind: "queued"; job: Job };

function StatusBanners({ status, onRetryStart, retrying }: { status: Status | null; onRetryStart: () => void; retrying: boolean }): React.ReactElement | null {
  const { t } = useTranslation();
  if (status === null) {
    return null;
  }
  const detail = (message: string): React.ReactElement => (
    <span className="block whitespace-pre-wrap break-words">{message}</span>
  );
  switch (status.kind) {
    case "checked":
      return <Banner tone="info" title={t("apps.compose.checked")} description={t("apps.compose.checkedDescription")} />;
    case "checkFailed":
      return (
        <Banner
          tone="error"
          title={status.code === INVALID_STACK_CODE ? t("apps.compose.invalid") : t("apps.compose.checkFailed")}
          description={detail(status.message)}
        />
      );
    case "saveFailed":
      return (
        <Banner
          tone="error"
          title={status.code === INVALID_STACK_CODE ? t("apps.compose.notAppliedInvalid") : t("apps.compose.saveFailed")}
          description={detail(status.message)}
        />
      );
    case "notStarted":
      return (
        <Banner
          tone="warning"
          title={t("apps.compose.notStarted")}
          description={
            <>
              <span className="block">{t("apps.compose.notStartedDescription")}</span>
              {detail(status.message)}
            </>
          }
          action={
            <Button size="xs" variant="outline" loading={retrying} onClick={onRetryStart}>
              {t("apps.compose.retryStart")}
            </Button>
          }
        />
      );
    case "queued":
      return null;
  }
}

function JobFollow({ queued, stackName }: { queued: Job; stackName: string }): React.ReactElement {
  const { t } = useTranslation();
  const [finished, setFinished] = useState(FINISHED.includes(queued.status));
  const query = useApiQuery<Job>({
    queryKey: ["compose-job", queued.id],
    queryFn: async (signal) => {
      const result = await getJob(queued.id, signal);
      if (result.data !== undefined && FINISHED.includes(result.data.status)) {
        setFinished(true);
      }
      return result;
    },
    pollIntervalMs: finished ? undefined : JOB_POLL_MS,
    fallbackError: t("apps.compose.job.followFailed"),
  });
  const job = query.data ?? queued;
  const done = FINISHED.includes(job.status);

  return (
    <div className="flex flex-col gap-3 rounded-xl border p-4">
      <p className="text-sm font-medium">{t("apps.compose.job.title", { name: stackName })}</p>
      <JobProgress job={job} />
      {job.status === "succeeded" ? <Banner tone="info" title={t("apps.compose.job.done")} /> : null}
      {done && job.status !== "succeeded" ? (
        <Banner tone="error" title={t("apps.compose.job.failed")} description={job.error?.message} />
      ) : null}
      {query.error ? <Banner tone="error" title={query.error} /> : null}
      <Link to={jobDetailPath(job.id)} className={buttonVariants({ variant: "outline", className: "self-start" })}>
        {t("apps.compose.job.view")}
      </Link>
    </div>
  );
}

function ComposeEditor({ initial, compose }: { initial: Stack; compose: string }): React.ReactElement {
  const { t } = useTranslation();
  const [stack, setStack] = useState(initial);
  const [text, setText] = useState(compose);
  const [baseline, setBaseline] = useState(compose);
  const [status, setStatus] = useState<Status | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [importOpen, setImportOpen] = useState(false);

  const [phase, setPhase] = useState<"idle" | "validating" | "saving" | "starting">("idle");

  const dirty = text !== baseline;
  const busy = phase !== "idle";

  const blocker = useBlocker(
    ({ currentLocation, nextLocation }) => dirty && currentLocation.pathname !== nextLocation.pathname,
  );

  useEffect(() => {
    if (!dirty) {
      return;
    }
    const warn = (event: BeforeUnloadEvent): void => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);

  function edit(next: string): void {
    setText(next);
    setStatus((current) =>
      current?.kind === "checked" || current?.kind === "checkFailed" || current?.kind === "saveFailed"
        ? null
        : current,
    );
  }

  async function handleValidate(): Promise<void> {
    setStatus(null);
    setPhase("validating");
    try {
      const result = await call(() => updateStack(stack.name, text, true), t("apps.compose.checkFailed"));
      if (result.ok) {
        setStatus({ kind: "checked" });
      } else if (!result.aborted) {
        setStatus({ kind: "checkFailed", code: result.code, message: result.message });
      }
    } finally {
      setPhase("idle");
    }
  }

  async function handleStart(): Promise<void> {
    setPhase("starting");
    try {
      const result = await call(() => startStack(stack.name), t("apps.compose.startFailed"));
      if (result.ok && result.data !== undefined) {
        setStatus({ kind: "queued", job: result.data });
      } else if (result.ok) {
        setStatus({ kind: "notStarted", message: t("apps.compose.startFailed") });
      } else if (!result.aborted) {
        setStatus({ kind: "notStarted", message: result.message });
      }
    } finally {
      setPhase("idle");
    }
  }

  async function handleApply(): Promise<void> {
    setStatus(null);
    const submitted = text;
    setPhase("saving");
    const stored = await call(() => updateStack(stack.name, submitted, false), t("apps.compose.saveFailed"));
    if (!stored.ok || stored.data === undefined) {
      setPhase("idle");
      setConfirmOpen(false);
      if (!stored.ok && !stored.aborted) {
        setStatus({ kind: "saveFailed", code: stored.code, message: stored.message });
      } else if (stored.ok) {
        setStatus({ kind: "saveFailed", message: t("apps.compose.saveFailed") });
      }
      return;
    }
    setBaseline(submitted);
    setStack(stored.data.stack);
    await handleStart();
    setConfirmOpen(false);
  }

  const startedJob = status?.kind === "queued" ? status.job : null;

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-2">
        {stack.manuallyEdited ? <StatusBadge tone="warning">{t("apps.compose.manuallyEdited")}</StatusBadge> : null}
      </div>
      {stack.manuallyEdited ? (
        <p className="text-muted-foreground text-sm">{t("apps.compose.manuallyEditedHelp")}</p>
      ) : null}

      <Frame>
        <FramePanel className="flex flex-col gap-3 p-3">
          <Toolbar aria-label={t("apps.compose.toolbar")} className="flex-wrap items-center">
            <ToolbarButton
              disabled={busy}
              focusableWhenDisabled={false}
              onClick={() => void handleValidate()}
              render={<Button variant="outline" loading={phase === "validating"} />}
            >
              {t("apps.compose.validate")}
            </ToolbarButton>
            <ToolbarButton
              disabled={busy}
              focusableWhenDisabled={false}
              onClick={() => setImportOpen(true)}
              render={<Button variant="outline" />}
            >
              {t("apps.compose.import.action")}
            </ToolbarButton>
            <ToolbarButton
              disabled={busy}
              focusableWhenDisabled={false}
              onClick={() => setConfirmOpen(true)}
              render={<Button />}
            >
              {t("apps.compose.apply")}
            </ToolbarButton>
          </Toolbar>
          <CodeEditor value={text} onChange={edit} label={t("apps.compose.editor")} readOnly={busy} />
          {dirty ? <p className="text-muted-foreground text-sm">{t("apps.compose.unapplied")}</p> : null}
        </FramePanel>
      </Frame>

      <StatusBanners
        status={status}
        retrying={phase === "starting"}
        onRetryStart={() => void handleStart()}
      />
      {startedJob !== null ? <JobFollow queued={startedJob} stackName={stack.name} /> : null}

      <ConfirmDialog
        open={confirmOpen}
        onOpenChange={(next) => {
          if (!busy) {
            setConfirmOpen(next);
          }
        }}
        title={t("apps.compose.confirm.title", { name: stack.name })}
        description={t("apps.compose.confirm.description")}
        confirmLabel={t("apps.compose.apply")}
        loading={phase === "saving" || phase === "starting"}
        onConfirm={() => void handleApply()}
      />
      {importOpen ? (
        <ImportDialog
          onClose={() => setImportOpen(false)}
          onImport={(imported) => {
            edit(imported);
            setStatus(null);
          }}
        />
      ) : null}
      <UnsavedGuardDialog
        open={blocker.state === "blocked"}
        onStay={() => blocker.reset?.()}
        onDiscard={() => blocker.proceed?.()}
      />
    </div>
  );
}

export function ComposePage(): React.ReactElement {
  const { t } = useTranslation();
  const { name = "" } = useParams();
  const loadFailed = t("apps.compose.loadFailed");
  const query = useApiQuery<Loaded>({
    queryKey: ["compose", name],
    queryFn: (signal) => loadStack(name, loadFailed, signal),
    fallbackError: loadFailed,
  });
  const loaded = query.data;

  const backLink = (
    <Link to={appDetailPath(name)} className={buttonVariants({ variant: "ghost", size: "sm", className: "self-start" })}>
      <ArrowLeft aria-hidden="true" />
      {t("apps.compose.back", { name })}
    </Link>
  );

  if (loaded === null) {
    return (
      <div className="flex flex-col gap-4">
        {backLink}
        {query.error ? (
          <Banner
            tone="error"
            title={query.error}
            action={
              <Button size="xs" variant="outline" onClick={() => void query.refresh()}>
                {t("apps.installed.retry")}
              </Button>
            }
          />
        ) : (
          <LoadingBlock />
        )}
      </div>
    );
  }

  if (loaded.kind === "notStack") {
    return (
      <div className="flex flex-col gap-4">
        {backLink}
        <EmptyState
          icon={FileX}
          title={t("apps.compose.notStack.title")}
          description={t("apps.compose.notStack.description", { name })}
          action={
            <Link to={appDetailPath(name)} className={buttonVariants()}>
              {t("apps.compose.notStack.action", { name })}
            </Link>
          }
        />
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      {backLink}
      <div className="min-w-0">
        <h1 className="break-all text-2xl font-semibold font-heading">
          {t("apps.compose.title", { name: loaded.stack.name })}
        </h1>
        <p className="text-muted-foreground">{t("apps.compose.description")}</p>
      </div>
      <ComposeEditor key={loaded.stack.name} initial={loaded.stack} compose={loaded.compose} />
    </div>
  );
}
