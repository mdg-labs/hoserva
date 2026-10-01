import { Download } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { LogView } from "@/components/patterns/log-view";
import { SegmentedChoice } from "@/components/patterns/segmented-choice";
import { SettingSwitch } from "@/components/patterns/setting-switch";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { getAppLogs, streamAppLogs } from "@/lib/api/operations";
import { apiErrorMessage, isAbortError } from "@/lib/api/request";
import { appendLog, DEFAULT_LOG_LINES, LOG_LINE_CHOICES } from "@/routes/apps/logs";
import type { App } from "@/routes/apps/containers";

const NOT_FOUND_CODE = "app_not_found";
const LINES_FIELD = "app-log-lines";

type Failure = { code?: string; message: string };
type Phase = "loading" | "snapshot" | "live" | "ended" | "error";
type LogState = { key: string; text: string; phase: Phase; failure: Failure | null };

function saveText(text: string, filename: string): void {
  const url = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
}

// Reads the container's log: the last `tail` lines once, or, while `follow`
// is on, those lines and then every new one until the effect is cleaned up.
// A state belongs to the request that wrote it, so a request for another
// container, length or attempt starts over from loading.
function useAppLog(id: string, tail: number, follow: boolean, attempt: number): LogState {
  const key = `${id}|${tail}|${follow}|${attempt}`;
  const [state, setState] = useState<LogState>({ key, text: "", phase: "loading", failure: null });

  useEffect(() => {
    const controller = new AbortController();
    const { signal } = controller;
    let text = "";
    const publish = (phase: Phase, failure: Failure | null = null): void => {
      if (!signal.aborted) {
        setState({ key, text, phase, failure });
      }
    };

    void (async () => {
      try {
        if (!follow) {
          const result = await getAppLogs(id, tail, signal);
          if (result.error !== undefined || result.response?.ok === false) {
            publish("error", { code: result.error?.code, message: apiErrorMessage(result.error) });
            return;
          }
          text = result.data ?? "";
          publish("snapshot");
          return;
        }
        const result = await streamAppLogs(id, tail, signal);
        if (result.error !== undefined || result.response?.ok === false) {
          publish("error", { code: result.error?.code, message: apiErrorMessage(result.error) });
          return;
        }
        const reader = result.data?.getReader();
        if (reader === undefined) {
          publish("ended");
          return;
        }
        const decoder = new TextDecoder();
        publish("live");
        for (;;) {
          const chunk = await reader.read();
          if (chunk.done) {
            break;
          }
          text = appendLog(text, decoder.decode(chunk.value, { stream: true }));
          publish("live");
        }
        text = appendLog(text, decoder.decode());
        publish("ended");
      } catch (err: unknown) {
        if (!isAbortError(err, signal)) {
          publish("error", { message: apiErrorMessage(undefined, err instanceof Error ? err.message : undefined) });
        }
      }
    })();

    return () => controller.abort();
  }, [id, tail, follow, key]);

  return state.key === key ? state : { key, text: "", phase: "loading", failure: null };
}

export function LogsTab({ app }: { app: App }): React.ReactElement {
  const { t } = useTranslation();
  const [tail, setTail] = useState<number>(DEFAULT_LOG_LINES);
  const [follow, setFollow] = useState(false);
  const [search, setSearch] = useState("");
  const [attempt, setAttempt] = useState(0);
  const log = useAppLog(app.id, tail, follow, attempt);

  const retry = (
    <Button size="xs" variant="outline" onClick={() => setAttempt((value) => value + 1)}>
      {follow ? t("apps.logs.reconnect") : t("apps.installed.retry")}
    </Button>
  );
  const failure = log.failure;
  const hidden = log.phase === "error" && log.text === "";

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-3 sm:flex-row sm:flex-wrap sm:items-center">
        <SegmentedChoice
          name={LINES_FIELD}
          value={String(tail)}
          onChange={(value) => setTail(Number(value))}
          options={LOG_LINE_CHOICES.map((lines) => ({
            value: String(lines),
            label: t("apps.logs.lines", { count: lines }),
          }))}
        />
        <SettingSwitch
          label={t("apps.logs.follow")}
          checked={follow}
          onCheckedChange={setFollow}
          className="sm:w-auto"
        />
        <Input
          type="search"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          aria-label={t("apps.logs.search")}
          placeholder={t("apps.logs.search")}
          className="sm:max-w-64"
        />
        <Button
          variant="outline"
          disabled={log.text === ""}
          onClick={() => saveText(log.text, t("apps.logs.filename", { name: app.name }))}
        >
          <Download aria-hidden="true" />
          {t("apps.logs.download")}
        </Button>
      </div>
      {failure !== null ? (
        <Banner
          tone="error"
          title={failure.code === NOT_FOUND_CODE ? t("apps.logs.notFound", { name: app.name }) : t("apps.logs.loadFailed")}
          description={failure.code === NOT_FOUND_CODE ? undefined : failure.message}
          action={failure.code === NOT_FOUND_CODE ? undefined : retry}
        />
      ) : null}
      {log.phase === "ended" ? (
        <Banner
          tone="info"
          title={t("apps.logs.ended")}
          description={t("apps.logs.endedDescription")}
          action={retry}
        />
      ) : null}
      {hidden ? null : (
        <LogView
          content={log.text === "" && log.phase !== "loading" ? t("apps.logs.empty") : log.text}
          loading={log.phase === "loading"}
          highlight={log.text === "" ? "" : search}
          followEnd={follow}
        />
      )}
    </div>
  );
}
