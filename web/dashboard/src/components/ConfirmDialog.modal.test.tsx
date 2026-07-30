import { useState } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ConfirmDialog } from "@/components/ConfirmDialog";

type HarnessProps = {
  showCancel?: boolean;
  removeTriggerOnConfirm?: boolean;
  onConfirm?: () => void;
  onCancel?: () => void;
};

function DialogHarness({
  showCancel = true,
  removeTriggerOnConfirm = false,
  onConfirm = vi.fn(),
  onCancel = vi.fn(),
}: HarnessProps) {
  const [open, setOpen] = useState(false);
  const [showTrigger, setShowTrigger] = useState(true);

  return (
    <>
      {showTrigger ? (
        <button type="button" onClick={() => setOpen(true)}>
          Open confirmation
        </button>
      ) : null}
      <button type="button">Background action</button>
      <ConfirmDialog
        open={open}
        title="Delete data?"
        confirmLabel="Delete"
        showCancel={showCancel}
        danger
        onConfirm={() => {
          onConfirm();
          setOpen(false);
          if (removeTriggerOnConfirm) setShowTrigger(false);
        }}
        onCancel={() => {
          onCancel();
          setOpen(false);
        }}
      >
        This cannot be undone.
      </ConfirmDialog>
    </>
  );
}

function openDialog() {
  const trigger = screen.getByRole("button", { name: "Open confirmation" });
  trigger.focus();
  fireEvent.click(trigger);
  return trigger;
}

afterEach(() => cleanup());

describe("ConfirmDialog keyboard modal contract", () => {
  it("uses Cancel as the safe initial focus and traps forward/backward Tab", () => {
    render(<DialogHarness />);
    openDialog();

    const cancel = screen.getByRole("button", { name: "Cancel" });
    const confirm = screen.getByRole("button", { name: "Delete" });
    expect(document.activeElement).toBe(cancel);

    confirm.focus();
    fireEvent.keyDown(document, { key: "Tab" });
    expect(document.activeElement).toBe(cancel);

    cancel.focus();
    fireEvent.keyDown(document, { key: "Tab", shiftKey: true });
    expect(document.activeElement).toBe(confirm);
  });

  it("falls back to Confirm when there is no cancel action", () => {
    render(<DialogHarness showCancel={false} />);
    openDialog();

    expect(document.activeElement).toBe(
      screen.getByRole("button", { name: "Delete" }),
    );
  });

  it("makes background content inert and hidden only while open", () => {
    const { container } = render(<DialogHarness />);
    openDialog();

    expect(container.hasAttribute("inert")).toBe(true);
    expect(container.getAttribute("aria-hidden")).toBe("true");

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(container.hasAttribute("inert")).toBe(false);
    expect(container.hasAttribute("aria-hidden")).toBe(false);
  });

  it.each([
    ["cancel", () => fireEvent.click(screen.getByRole("button", { name: "Cancel" }))],
    ["confirm", () => fireEvent.click(screen.getByRole("button", { name: "Delete" }))],
    ["Escape", () => fireEvent.keyDown(document, { key: "Escape" })],
  ])("restores the initiating element after %s", (_name, close) => {
    render(<DialogHarness />);
    const trigger = openDialog();

    close();

    expect(document.activeElement).toBe(trigger);
  });

  it("does not focus a trigger removed while confirming", () => {
    render(<DialogHarness removeTriggerOnConfirm />);
    const trigger = openDialog();

    fireEvent.click(screen.getByRole("button", { name: "Delete" }));

    expect(trigger.isConnected).toBe(false);
    expect(document.activeElement).not.toBe(trigger);
  });

  it("exempts toast containers with aria-live from background inert state", () => {
    const toast = document.createElement("div");
    toast.setAttribute("data-toast-container", "true");
    toast.setAttribute("aria-live", "polite");
    document.body.appendChild(toast);

    try {
      render(<DialogHarness />);
      openDialog();
      expect(toast.hasAttribute("inert")).toBe(false);
      expect(toast.hasAttribute("aria-hidden")).toBe(false);
    } finally {
      toast.remove();
    }
  });

  it("contains body siblings dynamically appended after modal opens", async () => {
    render(<DialogHarness />);
    openDialog();

    const sibling = document.createElement("div");
    sibling.id = "dynamic-sibling";
    document.body.appendChild(sibling);

    await vi.waitFor(() => {
      expect(sibling.hasAttribute("inert")).toBe(true);
      expect(sibling.getAttribute("aria-hidden")).toBe("true");
    });

    sibling.remove();
  });

  it("retains focus within panel when action becomes busy", () => {
    const { rerender } = render(
      <ConfirmDialog
        open={true}
        title="Busy Test"
        busy={false}
        onConfirm={vi.fn()}
        onCancel={vi.fn()}
      >
        Body
      </ConfirmDialog>,
    );

    const confirm = screen.getByRole("button", { name: "Confirm" });
    confirm.focus();
    expect(document.activeElement).toBe(confirm);

    rerender(
      <ConfirmDialog
        open={true}
        title="Busy Test"
        busy={true}
        onConfirm={vi.fn()}
        onCancel={vi.fn()}
      >
        Body
      </ConfirmDialog>,
    );

    expect(confirm.disabled).toBe(true);
    const dialog = screen.getByRole("dialog");
    expect(dialog.contains(document.activeElement)).toBe(true);
  });
});
