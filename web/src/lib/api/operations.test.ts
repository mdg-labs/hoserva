import { gzipSync } from "node:zlib";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

// createHoservaClient captures globalThis.fetch and Request when
// operations.ts is first imported, so the stubs have to be in place before
// that import is evaluated. Node's Request rejects the client's relative
// "/api/v1" base URL, which a browser resolves against the page.
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
  globalThis.fetch = ((input: Request) => {
    if (!respond.current) {
      throw new Error("no fetch responder installed");
    }
    return Promise.resolve(respond.current(input));
  }) as typeof fetch;
});

let getJobLog: typeof import("@/lib/api/operations").getJobLog;

beforeAll(async () => {
  ({ getJobLog } = await import("@/lib/api/operations"));
});

afterEach(() => {
  respond.current = undefined;
});

afterAll(() => {
  globalThis.fetch = originals.fetch;
  globalThis.Request = originals.Request;
});

const JOB_ID = "6f1f7c1e-3b0a-4f7e-9d54-2f2f0a6b8c11";

function gzipResponse(text: string): Response {
  return new Response(new Uint8Array(gzipSync(Buffer.from(text))), {
    status: 200,
    headers: { "Content-Type": "application/gzip" },
  });
}

describe("getJobLog", () => {
  it("decompresses the gzip body the API declares and returns the log text", async () => {
    respond.current = () => gzipResponse("sync started\nsync finished — 3 files\n");

    const result = await getJobLog(JOB_ID);

    expect(result.error).toBeUndefined();
    expect(result.response?.ok).toBe(true);
    expect(result.data).toBe("sync started\nsync finished — 3 files\n");
  });

  it("requests the log of the job it was asked for", async () => {
    let seen = "";
    respond.current = (request) => {
      seen = new URL(request.url).pathname;
      return gzipResponse("x");
    };

    await getJobLog(JOB_ID);

    expect(seen).toBe(`/api/v1/jobs/${JOB_ID}/log`);
  });

  it("returns the text of a body the browser already decompressed", async () => {
    respond.current = () =>
      new Response("already plain\n", { status: 200, headers: { "Content-Type": "application/gzip" } });

    const result = await getJobLog(JOB_ID);

    expect(result.data).toBe("already plain\n");
  });

  it("keeps the output of a log that has no gzip trailer yet because its job is still running", async () => {
    const full = gzipSync(Buffer.from("line one\nline two\n".repeat(200)));
    const truncated = new Uint8Array(full.subarray(0, full.length - 8));
    respond.current = () => new Response(truncated, { status: 200, headers: { "Content-Type": "application/gzip" } });

    const result = await getJobLog(JOB_ID);

    expect(result.error).toBeUndefined();
    expect(result.data).toContain("line one\nline two\n");
  });

  it("reports corrupt gzip data that yields no output as a failure", async () => {
    respond.current = () =>
      new Response(new Uint8Array([0x1f, 0x8b, 0x08, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff]), {
        status: 200,
        headers: { "Content-Type": "application/gzip" },
      });

    await expect(getJobLog(JOB_ID)).rejects.not.toBeInstanceOf(SyntaxError);
    await expect(getJobLog(JOB_ID)).rejects.toBeDefined();
  });

  it("treats a job with no captured log as an empty log, not a failure", async () => {
    respond.current = () =>
      Response.json(
        { code: "job_log_not_found", message: `job ${JOB_ID} has no captured log` },
        { status: 404 },
      );

    const result = await getJobLog(JOB_ID);

    expect(result.error).toBeUndefined();
    expect(result.response?.ok).toBe(true);
    expect(result.data).toBeUndefined();
  });

  it("passes any other API error through", async () => {
    respond.current = () =>
      Response.json({ code: "job_not_found", message: "no job with that id" }, { status: 404 });

    const result = await getJobLog(JOB_ID);

    expect(result.error?.code).toBe("job_not_found");
    expect(result.response?.ok).toBe(false);
  });

  it("returns undefined data for an empty 200 body", async () => {
    respond.current = () => new Response(null, { status: 200, headers: { "Content-Length": "0" } });

    const result = await getJobLog(JOB_ID);

    expect(result.error).toBeUndefined();
    expect(result.data).toBeUndefined();
  });
});
