import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { App } from "@/routes/apps/containers";
import { LogsTab } from "@/routes/apps/logs-tab";

const mockGet = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: { GET: (...args: unknown[]) => mockGet(...args) },
}));

const running = { id: "id-web", name: "web", state: "running" } as App;
const stopped = { id: "id-web", name: "web", state: "exited" } as App;

type Call = { path: string; query: { tail: number; follow?: boolean }; parseAs?: string; signal: AbortSignal };

function calls(): Call[] {
  return mockGet.mock.calls.map(([path, options]) => ({
    path,
    query: options.params.query,
    parseAs: options.parseAs,
    signal: options.signal,
  }));
}

function ok<T>(data: T) {
  return Promise.resolve({ data, response: { ok: true } });
}

function fail(code: string, message: string) {
  return Promise.resolve({ error: { code, message }, response: { ok: false } });
}

function readBlob(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(reader.error);
    reader.readAsText(blob);
  });
}

function controlledStream() {
  let controller!: ReadableStreamDefaultController<Uint8Array>;
  const stream = new ReadableStream<Uint8Array>({
    start(c) {
      controller = c;
    },
  });
  const encoder = new TextEncoder();
  return {
    stream,
    push: (text: string) => controller.enqueue(encoder.encode(text)),
    close: () => controller.close(),
    error: (err: Error) => controller.error(err),
  };
}

beforeEach(() => {
  mockGet.mockReset();
});

afterEach(() => {
  cleanup();
});

describe("Logs tab", () => {
  it("loads the chosen number of lines and reloads when another length is chosen", async () => {
    mockGet.mockImplementation(() => ok("line one\nline two\n"));
    render(<LogsTab app={running} />);

    expect(await screen.findByText(/line one/)).toBeInTheDocument();
    expect(calls()[0]).toMatchObject({ path: "/apps/{id}/logs", query: { tail: 500 }, parseAs: "text" });

    fireEvent.click(screen.getByRole("radio", { name: "100 lines" }));
    await waitFor(() => expect(calls().at(-1)?.query.tail).toBe(100));
    expect(calls().at(-1)?.query.follow).toBeUndefined();
  });

  it("still shows the last lines of a stopped container", async () => {
    mockGet.mockImplementation(() => ok("last words\n"));
    render(<LogsTab app={stopped} />);

    expect(await screen.findByText(/last words/)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("names an app that is no longer there", async () => {
    mockGet.mockImplementation(() => fail("app_not_found", "no container web"));
    render(<LogsTab app={running} />);

    expect(await screen.findByText(/There is no app named web any more/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Try again" })).not.toBeInTheDocument();
  });

  it("shows a failed load with a way to try again", async () => {
    mockGet.mockImplementationOnce(() => fail("docker_unavailable", "Docker is not running"));
    mockGet.mockImplementation(() => ok("back again\n"));
    render(<LogsTab app={running} />);

    expect(await screen.findByText("Docker is not running")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText(/back again/)).toBeInTheDocument();
  });

  it("follows the log as a stream while the switch is on and stops when it is turned off", async () => {
    const live = controlledStream();
    mockGet.mockImplementation((_path: string, options: { params: { query: { follow?: boolean } } }) =>
      options.params.query.follow ? ok(live.stream) : ok("old\n"),
    );
    render(<LogsTab app={running} />);
    await screen.findByText(/old/);

    fireEvent.click(screen.getByRole("switch", { name: "Follow new output" }));
    await waitFor(() => expect(calls().at(-1)).toMatchObject({ query: { tail: 500, follow: true }, parseAs: "stream" }));
    const streamSignal = calls().at(-1)?.signal as AbortSignal;

    await act(async () => live.push("first\n"));
    expect(await screen.findByText(/first/)).toBeInTheDocument();
    await act(async () => live.push("second\n"));
    expect(await screen.findByText(/second/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("switch", { name: "Follow new output" }));
    await waitFor(() => expect(streamSignal.aborted).toBe(true));
    await waitFor(() => expect(calls().at(-1)?.query.follow).toBeUndefined());
  });

  it("keeps the newest line in view while following", async () => {
    const live = controlledStream();
    mockGet.mockImplementationOnce(() => ok("")).mockImplementation(() => ok(live.stream));
    const height = vi.spyOn(HTMLElement.prototype, "scrollHeight", "get").mockReturnValue(1234);
    try {
      render(<LogsTab app={running} />);
      fireEvent.click(screen.getByRole("switch", { name: "Follow new output" }));
      await waitFor(() => expect(calls().length).toBe(2));

      await act(async () => live.push("newest\n"));
      await screen.findByText(/newest/);

      const viewport = document.querySelector("[data-slot=scroll-area-viewport]") as HTMLElement;
      expect(viewport.scrollTop).toBe(1234);
    } finally {
      height.mockRestore();
    }
  });

  it("stops reading the stream when the tab closes", async () => {
    const live = controlledStream();
    mockGet.mockImplementation(() => ok(live.stream));
    const view = render(<LogsTab app={running} />);
    fireEvent.click(screen.getByRole("switch", { name: "Follow new output" }));
    await waitFor(() => expect(calls().some((call) => call.query.follow === true)).toBe(true));
    const streamSignal = calls().find((call) => call.query.follow === true)?.signal as AbortSignal;

    view.unmount();
    expect(streamSignal.aborted).toBe(true);
  });

  it("shows a broken stream with its lines so far and Reconnect", async () => {
    const live = controlledStream();
    const next = controlledStream();
    mockGet.mockImplementationOnce(() => ok("")).mockImplementationOnce(() => ok(live.stream));
    mockGet.mockImplementation(() => ok(next.stream));
    render(<LogsTab app={running} />);
    fireEvent.click(screen.getByRole("switch", { name: "Follow new output" }));
    await waitFor(() => expect(calls().length).toBe(2));

    await act(async () => live.push("before the break\n"));
    await screen.findByText(/before the break/);
    await act(async () => live.error(new Error("connection reset")));

    expect(await screen.findByText("connection reset")).toBeInTheDocument();
    expect(screen.getByText(/before the break/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Reconnect" }));
    await waitFor(() => expect(calls().length).toBe(3));
    await act(async () => next.push("after reconnect\n"));
    expect(await screen.findByText(/after reconnect/)).toBeInTheDocument();
    expect(screen.queryByText("connection reset")).not.toBeInTheDocument();
  });

  it("says so when the stream ends on its own", async () => {
    const live = controlledStream();
    mockGet.mockImplementationOnce(() => ok("")).mockImplementation(() => ok(live.stream));
    render(<LogsTab app={running} />);
    fireEvent.click(screen.getByRole("switch", { name: "Follow new output" }));
    await waitFor(() => expect(calls().length).toBe(2));

    await act(async () => {
      live.push("goodbye\n");
      live.close();
    });
    expect(await screen.findByText("The live output stopped")).toBeInTheDocument();
    expect(screen.getByText(/goodbye/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reconnect" })).toBeInTheDocument();
  });

  it("highlights the search text in the loaded lines, ignoring case", async () => {
    mockGet.mockImplementation(() => ok("ERROR: disk full\nall fine\nerror again\n"));
    render(<LogsTab app={running} />);
    await screen.findByText(/disk full/);

    fireEvent.change(screen.getByRole("searchbox", { name: "Search the loaded lines" }), {
      target: { value: "error" },
    });

    const marks = Array.from(document.querySelectorAll("mark")).map((mark) => mark.textContent);
    expect(marks).toEqual(["ERROR", "error"]);
  });

  it("downloads the loaded text under the app's name", async () => {
    mockGet.mockImplementation(() => ok("kept line\n"));
    const createObjectURL = vi.fn(() => "blob:logs");
    const revokeObjectURL = vi.fn();
    Object.defineProperty(URL, "createObjectURL", { configurable: true, writable: true, value: createObjectURL });
    Object.defineProperty(URL, "revokeObjectURL", { configurable: true, writable: true, value: revokeObjectURL });
    const names: string[] = [];
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      names.push(this.download);
    });
    render(<LogsTab app={running} />);
    await screen.findByText(/kept line/);

    fireEvent.click(screen.getByRole("button", { name: "Download" }));

    expect(createObjectURL).toHaveBeenCalledTimes(1);
    const blob = (createObjectURL.mock.calls[0] as unknown as [Blob])[0];
    expect(await readBlob(blob)).toBe("kept line\n");
    expect(names).toEqual(["web-logs.txt"]);
    expect(revokeObjectURL).toHaveBeenCalledWith("blob:logs");
    click.mockRestore();
  });

  it("offers no download while there is nothing loaded", async () => {
    mockGet.mockImplementation(() => ok(""));
    render(<LogsTab app={running} />);

    expect(await screen.findByText("This app has not written any output yet.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Download" })).toBeDisabled();
  });
});
