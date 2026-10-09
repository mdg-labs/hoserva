import { sessionHeaders, type components } from "@/lib/api/client";

export type StreamEvent = components["schemas"]["Event"];
export type NotificationAlert = components["schemas"]["NotificationAlert"];
export type NotificationGroup = components["schemas"]["NotificationGroup"];

const EVENTS_URL = "/api/v1/events";
const RECONNECT_DELAY_MS = 3000;

// The stream is read with fetch, not EventSource: an EventSource cannot send
// the session's second secret, and the session cookie alone does not
// authenticate a request (doc 15 T15). A dropped connection is reopened
// after a delay, as an EventSource would; a refusal (an expired or revoked
// session, a role that no longer reads the stream) is not retried.
export function subscribeToEvents(
  onEvent: (event: StreamEvent) => void,
  onError?: (error: Event) => void,
): () => void {
  const controller = new AbortController();
  const { signal } = controller;

  const run = async (): Promise<void> => {
    while (!signal.aborted) {
      try {
        const response = await fetch(EVENTS_URL, {
          headers: { Accept: "text/event-stream", ...sessionHeaders() },
          credentials: "same-origin",
          cache: "no-store",
          signal,
        });
        if (!response.ok || response.body === null) {
          onError?.(new Event("error"));
          return;
        }
        await readFrames(response.body, onEvent, signal);
      } catch {
        if (signal.aborted) {
          return;
        }
      }
      if (signal.aborted) {
        return;
      }
      onError?.(new Event("error"));
      await pause(RECONNECT_DELAY_MS, signal);
    }
  };
  void run();

  return () => {
    controller.abort();
  };
}

async function readFrames(
  body: ReadableStream<Uint8Array>,
  onEvent: (event: StreamEvent) => void,
  signal: AbortSignal,
): Promise<void> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffered = "";
  try {
    while (!signal.aborted) {
      const { done, value } = await reader.read();
      if (done) {
        return;
      }
      buffered += decoder.decode(value, { stream: true }).replace(/\r\n?/g, "\n");
      let end = buffered.indexOf("\n\n");
      while (end !== -1) {
        dispatchFrame(buffered.slice(0, end), onEvent);
        buffered = buffered.slice(end + 2);
        end = buffered.indexOf("\n\n");
      }
    }
  } finally {
    await reader.cancel().catch(() => undefined);
  }
}

function dispatchFrame(frame: string, onEvent: (event: StreamEvent) => void): void {
  const data: string[] = [];
  for (const line of frame.split("\n")) {
    if (line.startsWith("data:")) {
      data.push(line.slice(line.startsWith("data: ") ? 6 : 5));
    }
  }
  if (data.length === 0) {
    return;
  }
  try {
    onEvent(JSON.parse(data.join("\n")) as StreamEvent);
  } catch {
    // Malformed SSE frames are ignored; the next event or a reload recovers.
  }
}

function pause(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(done, ms);
    signal.addEventListener("abort", done, { once: true });
    function done(): void {
      clearTimeout(timer);
      signal.removeEventListener("abort", done);
      resolve();
    }
  });
}
