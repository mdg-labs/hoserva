// @vitest-environment node
// Node's Request rejects the client's relative "/api/v1" base URL, which a
// browser resolves against the page, so fetch and Request are stubbed before
// the client module is evaluated (the same setup operations.test.ts uses).
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

const respond = vi.hoisted(() => ({
  current: undefined as undefined | ((request: Request) => Response | Promise<Response>),
}));
const originals = vi.hoisted(() => ({ fetch: globalThis.fetch, Request: globalThis.Request }));
vi.hoisted(() => {
  const NativeRequest = globalThis.Request;
  globalThis.Request = class extends NativeRequest {
    constructor(input: RequestInfo | URL, init?: RequestInit) {
      super(typeof input === "string" && input.startsWith("/") ? new URL(input, "http://hoserva.test").href : input, init);
    }
  };
  globalThis.fetch = ((input: Request | string, init?: RequestInit) => {
    if (!respond.current) {
      throw new Error("no fetch responder installed");
    }
    const request =
      typeof input === "string" ? new Request(new URL(input, "http://hoserva.test").href, init) : input;
    return Promise.resolve(respond.current(request));
  }) as typeof fetch;
});

let ops: typeof import("@/lib/api/operations");
let events: typeof import("@/lib/api/events");

beforeAll(async () => {
  ops = await import("@/lib/api/operations");
  events = await import("@/lib/api/events");
});

afterEach(() => {
  respond.current = undefined;
});

afterAll(() => {
  globalThis.fetch = originals.fetch;
  globalThis.Request = originals.Request;
});

const SECRET_HEADER = "X-Hoserva-Session-Secret";
const USER = { id: "00000000-0000-0000-0000-000000000001", username: "admin", role: "admin", totpEnrolled: false };

function loginResponse(secret: string): Response {
  return new Response(JSON.stringify(USER), {
    status: 200,
    headers: { "Content-Type": "application/json", [SECRET_HEADER]: secret },
  });
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

describe("the session's second secret", () => {
  it("is kept from the login response and sent on every later request until logout", async () => {
    respond.current = () => loginResponse("secret-one");
    await ops.postAuthLogin({ username: "admin", password: "pw" });

    const sent: Array<string | null> = [];
    respond.current = (request) => {
      sent.push(request.headers.get(SECRET_HEADER));
      return jsonResponse(request.method === "POST" ? {} : { jobs: [] });
    };
    await ops.getJobs();
    expect(sent).toEqual(["secret-one"]);

    respond.current = (request) => {
      sent.push(request.headers.get(SECRET_HEADER));
      return new Response(null, { status: 204 });
    };
    await ops.postAuthLogout();
    expect(sent).toEqual(["secret-one", "secret-one"]);

    respond.current = (request) => {
      sent.push(request.headers.get(SECRET_HEADER));
      return jsonResponse({ jobs: [] });
    };
    await ops.getJobs();
    expect(sent).toEqual(["secret-one", "secret-one", null]);
  });

  it("is not taken from a refused login", async () => {
    respond.current = () => jsonResponse({ code: "invalid_credentials", message: "no" }, 401);
    await ops.postAuthLogin({ username: "admin", password: "wrong" });

    let sent: string | null = "unset";
    respond.current = (request) => {
      sent = request.headers.get(SECRET_HEADER);
      return jsonResponse({ jobs: [] });
    };
    await ops.getJobs();
    expect(sent).toBeNull();
  });
});

describe("subscribeToEvents", () => {
  function streamOf(chunks: string[]): ReadableStream<Uint8Array> {
    const encoder = new TextEncoder();
    return new ReadableStream({
      start(controller) {
        for (const chunk of chunks) {
          controller.enqueue(encoder.encode(chunk));
        }
        controller.close();
      },
    });
  }

  it("reads frames over fetch with the session secret, however the chunks fall", async () => {
    respond.current = () => loginResponse("secret-two");
    await ops.postAuthLogin({ username: "admin", password: "pw" });

    const received: unknown[] = [];
    let sent: string | null = null;
    let calls = 0;
    const done = new Promise<void>((resolve) => {
      respond.current = (request) => {
        calls += 1;
        sent = request.headers.get(SECRET_HEADER);
        if (calls > 1) {
          resolve();
          return new Response(null, { status: 401 });
        }
        return new Response(
          streamOf([': keep-alive\n\ndata: {"event":"catalog","da', 'ta":{"n":1}}\r\n\r\ndata: not json\n\n']),
          { status: 200, headers: { "Content-Type": "text/event-stream" } },
        );
      };
    });
    vi.useFakeTimers();
    try {
      const stop = events.subscribeToEvents((event) => received.push(event));
      await vi.advanceTimersByTimeAsync(3000);
      await done;
      stop();
    } finally {
      vi.useRealTimers();
    }

    expect(sent).toBe("secret-two");
    expect(received).toEqual([{ event: "catalog", data: { n: 1 } }]);
  });

  it("does not reconnect after a refusal", async () => {
    let calls = 0;
    respond.current = () => {
      calls += 1;
      return new Response(null, { status: 401 });
    };
    vi.useFakeTimers();
    try {
      const onError = vi.fn();
      const stop = events.subscribeToEvents(() => undefined, onError);
      await vi.advanceTimersByTimeAsync(30_000);
      stop();
      expect(calls).toBe(1);
      expect(onError).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });
});
