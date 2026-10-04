// @vitest-environment node
// jsdom's FormData and File are not the ones Node's Request accepts as a body, so the
// multipart upload could not be sent through the client in a jsdom environment.
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
let ops: typeof import("@/lib/api/operations");

beforeAll(async () => {
  ops = await import("@/lib/api/operations");
  ({ getJobLog } = ops);
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

describe("migration operations", () => {
  const JOB = { id: JOB_ID, type: "migration_scan", status: "queued" };

  it("reads the session from /migrate", async () => {
    let seen = "";
    respond.current = (request) => {
      seen = `${request.method} ${new URL(request.url).pathname}`;
      return Response.json({ phase: "none", flashDevices: [], zipOnly: false });
    };

    const result = await ops.getMigration();

    expect(seen).toBe("GET /api/v1/migrate");
    expect(result.data?.phase).toBe("none");
  });

  it("passes the API error of a failed session read through", async () => {
    respond.current = () => Response.json({ code: "internal", message: "database is locked" }, { status: 500 });

    const result = await ops.getMigration();

    expect(result.data).toBeUndefined();
    expect(result.error?.message).toBe("database is locked");
  });

  it("deletes the session with DELETE /migrate", async () => {
    let seen = "";
    respond.current = (request) => {
      seen = `${request.method} ${new URL(request.url).pathname}`;
      return new Response(null, { status: 204 });
    };

    const result = await ops.forgetMigration();

    expect(seen).toBe("DELETE /api/v1/migrate");
    expect(result.error).toBeUndefined();
  });

  it("uploads the Flash Backup zip as the multipart `file` part", async () => {
    let seen = "";
    let part: FormDataEntryValue | null = null;
    respond.current = async (request) => {
      seen = `${request.method} ${new URL(request.url).pathname}`;
      part = (await request.formData()).get("file");
      return Response.json(JOB);
    };

    const result = await ops.startMigrationScan(new File(["zip bytes"], "flash.zip", { type: "application/zip" }));

    expect(seen).toBe("POST /api/v1/migrate/scan");
    expect(part).toBeInstanceOf(File);
    expect((part as unknown as File).name).toBe("flash.zip");
    expect(await (part as unknown as File).text()).toBe("zip bytes");
    expect(result.data?.id).toBe(JOB_ID);
  });

  it("passes the refusal of an upload through with its message", async () => {
    respond.current = () =>
      Response.json({ code: "invalid_zip", message: "the file is not a zip archive" }, { status: 400 });

    const result = await ops.startMigrationScan(new File(["nope"], "flash.zip"));

    expect(result.data).toBeUndefined();
    expect(result.error?.code).toBe("invalid_zip");
    expect(result.error?.message).toBe("the file is not a zip archive");
  });

  it("scans the stick by the device path as JSON", async () => {
    let seen = "";
    let body: unknown;
    respond.current = async (request) => {
      seen = `${request.method} ${new URL(request.url).pathname}`;
      body = await request.json();
      return Response.json(JOB);
    };

    await ops.startMigrationDeviceScan("/dev/sdu");

    expect(seen).toBe("POST /api/v1/migrate/scan/device");
    expect(body).toEqual({ device: "/dev/sdu" });
  });

  it("reads the report as the Markdown text it is", async () => {
    respond.current = () =>
      new Response("# Report\n\nverdict: no_go\n", { status: 200, headers: { "Content-Type": "text/markdown" } });

    const result = await ops.getMigrationReport();

    expect(result.error).toBeUndefined();
    expect(result.data).toBe("# Report\n\nverdict: no_go\n");
  });

  it("parses the JSON error of a report that does not exist yet", async () => {
    respond.current = () =>
      Response.json({ code: "no_migration_report", message: "no scan has finished" }, { status: 404 });

    const result = await ops.getMigrationReport();

    expect(result.data).toBeUndefined();
    expect(result.error?.code).toBe("no_migration_report");
  });

  it("lists the template preview from /migrate/templates", async () => {
    let seen = "";
    respond.current = (request) => {
      seen = new URL(request.url).pathname;
      return Response.json({ counts: {}, templates: [], composeProjects: [] });
    };

    const result = await ops.listMigrationTemplates();

    expect(seen).toBe("/api/v1/migrate/templates");
    expect(result.data?.templates).toEqual([]);
  });

  it("asks for one template by its file name, escaped in the path", async () => {
    let seen = "";
    respond.current = (request) => {
      seen = new URL(request.url).pathname;
      return Response.json({ kind: "template", name: "my notes.xml" });
    };

    await ops.getMigrationTemplate("my notes.xml");

    expect(seen).toBe("/api/v1/migrate/templates/my%20notes.xml");
  });
});
