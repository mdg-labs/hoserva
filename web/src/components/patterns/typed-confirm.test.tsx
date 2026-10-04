import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it } from "vitest";

import { TypedConfirm } from "@/components/patterns/typed-confirm";

const MISMATCH = "The confirmation phrase does not match.";

function Harness({ initial = "" }: { initial?: string }): React.ReactElement {
  const [value, setValue] = useState(initial);
  return <TypedConfirm phrase="DELETE" value={value} onChange={setValue} title="Confirm" items={[]} />;
}

describe("TypedConfirm", () => {
  afterEach(cleanup);

  it("shows no mismatch message and no invalid mark while the field is empty", () => {
    render(<Harness />);
    expect(screen.queryByText(MISMATCH)).toBeNull();
    expect(screen.getByRole("textbox").getAttribute("aria-invalid")).not.toBe("true");
  });

  it("shows the mismatch message and marks the control invalid for a wrong phrase", () => {
    render(<Harness />);
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "DELET" } });
    expect(screen.getByText(MISMATCH)).toBeTruthy();
    expect(screen.getByRole("textbox").getAttribute("aria-invalid")).toBe("true");
  });

  it("clears the message and the invalid mark once the phrase matches", () => {
    render(<Harness />);
    const input = screen.getByRole("textbox");
    fireEvent.change(input, { target: { value: "DELET" } });
    expect(screen.getByText(MISMATCH)).toBeTruthy();
    fireEvent.change(input, { target: { value: "DELETE" } });
    expect(screen.queryByText(MISMATCH)).toBeNull();
    expect(input.getAttribute("aria-invalid")).not.toBe("true");
  });

  it("shows no message for a matching phrase on first render", () => {
    render(<Harness initial="DELETE" />);
    expect(screen.queryByText(MISMATCH)).toBeNull();
  });
});
