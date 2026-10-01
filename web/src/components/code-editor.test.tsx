import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { CodeEditor } from "@/components/code-editor";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  delete (Range.prototype as Partial<Range>).getClientRects;
  delete (Range.prototype as Partial<Range>).getBoundingClientRect;
});

describe("CodeEditor without layout (jsdom)", () => {
  it("is a labelled textarea that reports edits", () => {
    const onChange = vi.fn();
    render(<CodeEditor value="services: {}" onChange={onChange} label="Compose" />);

    const box = screen.getByRole("textbox", { name: "Compose" });
    expect(box).toHaveValue("services: {}");
    fireEvent.change(box, { target: { value: "services: {a: {}}" } });
    expect(onChange).toHaveBeenCalledWith("services: {a: {}}");
  });

  it("can be made read-only", () => {
    render(<CodeEditor value="a: 1" onChange={vi.fn()} label="Compose" readOnly />);
    expect(screen.getByRole("textbox", { name: "Compose" })).toHaveAttribute("readonly");
  });
});

describe("CodeEditor in a browser", () => {
  function withLayout(): void {
    const rect = { x: 0, y: 0, top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0, toJSON: () => ({}) };
    Range.prototype.getClientRects = () => ({ length: 0, item: () => null, [Symbol.iterator]: [][Symbol.iterator] }) as unknown as DOMRectList;
    Range.prototype.getBoundingClientRect = () => rect as DOMRect;
  }

  it("mounts CodeMirror with the text and the accessible name, and follows the value prop", () => {
    withLayout();
    const { container, rerender } = render(<CodeEditor value={"services:\n  web: {}"} onChange={vi.fn()} label="Compose" />);

    const content = container.querySelector(".cm-content");
    expect(content).not.toBeNull();
    expect(content).toHaveAttribute("aria-label", "Compose");
    expect(content).toHaveAttribute("aria-multiline", "true");
    expect(content?.textContent).toContain("services:");
    expect(screen.queryByRole("textbox", { name: "Compose" })?.tagName).not.toBe("TEXTAREA");

    act(() => rerender(<CodeEditor value="name: other" onChange={vi.fn()} label="Compose" />));
    expect(container.querySelector(".cm-content")?.textContent).toContain("name: other");
    expect(container.querySelector(".cm-content")?.textContent).not.toContain("services:");
  });

  it("is read-only when asked", () => {
    withLayout();
    const { container } = render(<CodeEditor value="a: 1" onChange={vi.fn()} label="Compose" readOnly />);
    expect(container.querySelector(".cm-content")).toHaveAttribute("contenteditable", "true");
    expect(container.querySelector(".cm-content")).toHaveAttribute("aria-readonly", "true");
  });
});
