import type { components } from "@api/client";

export type StreamEvent = components["schemas"]["Event"];
export type NotificationAlert = components["schemas"]["NotificationAlert"];
export type NotificationGroup = components["schemas"]["NotificationGroup"];

export function subscribeToEvents(
  onEvent: (event: StreamEvent) => void,
  onError?: (error: Event) => void,
): () => void {
  const source = new EventSource("/api/v1/events");
  source.onmessage = (message) => {
    try {
      onEvent(JSON.parse(message.data) as StreamEvent);
    } catch {
      // Malformed SSE frames are ignored; the next event or a reload recovers.
    }
  };
  source.onerror = (error) => {
    onError?.(error);
  };
  return () => {
    source.close();
  };
}
