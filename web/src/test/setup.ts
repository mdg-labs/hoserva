import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach } from "vitest";

import "../lib/i18n";

// Vitest globals are off, so Testing Library registers no cleanup of its own.
// A test's last commit leaves React's passive-effect flush queued on
// setImmediate; letting it run here keeps it from firing after jsdom is
// torn down ("window is not defined", issue #539). The drain ends on a timer
// turn so the next test starts from the timers phase: Testing Library's
// findBy* settles with a setTimeout(0) while React schedules its renders on
// setImmediate, and a test that starts inside the check phase lets that timer
// win the race, so a findBy* can return a control that is still disabled.
afterEach(async () => {
  cleanup();
  await new Promise<void>((resolve) => setImmediate(resolve));
  await new Promise<void>((resolve) => setTimeout(resolve, 0));
});

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
