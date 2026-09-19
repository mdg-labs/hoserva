import "@testing-library/jest-dom/vitest";

import "../lib/i18n";

// jsdom does not implement EventSource. TopBar's inbox opens
// /api/v1/events on mount through subscribeToEvents, so every test that
// renders AppShell would throw ReferenceError without this.
class TestEventSource {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;

  readonly url: string;
  readonly withCredentials: boolean;
  readyState = TestEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;

  constructor(url: string | URL, init?: EventSourceInit) {
    this.url = String(url);
    this.withCredentials = init?.withCredentials ?? false;
    this.readyState = TestEventSource.OPEN;
  }

  close(): void {
    this.readyState = TestEventSource.CLOSED;
  }

  addEventListener(): void {}
  removeEventListener(): void {}
  dispatchEvent(): boolean {
    return false;
  }
}

Object.defineProperty(globalThis, "EventSource", {
  configurable: true,
  writable: true,
  value: TestEventSource,
});
