import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { TemplateIcon } from "@/components/patterns/template-icon";

afterEach(cleanup);

describe("TemplateIcon", () => {
  it("shows the neutral app glyph, not a letter, while no icon has loaded", () => {
    const { container } = render(<TemplateIcon id="no-icon" />);
    const fallback = container.querySelector('[data-slot="avatar-fallback"]');
    expect(fallback).not.toBeNull();
    expect(fallback?.getAttribute("aria-hidden")).toBe("true");
    expect(fallback?.querySelector("svg")).not.toBeNull();
    expect(fallback?.textContent).toBe("");
  });

  it("renders no image element until the icon has loaded, so a 404 leaves no broken image", () => {
    const { container } = render(<TemplateIcon id="no-icon" />);
    expect(container.querySelector("img")).toBeNull();
  });

  it("takes its colours from the avatar's theme tokens", () => {
    const { container } = render(<TemplateIcon id="no-icon" />);
    const avatar = container.querySelector('[data-slot="avatar"]');
    expect(avatar?.className).toContain("bg-muted");
    expect(container.querySelector("svg")?.getAttribute("class")).toContain("text-muted-foreground");
  });
});
