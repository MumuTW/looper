import { useState } from "react";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { ToastProvider, useToast } from "./toast";

function ToastConfirmationLifecycle() {
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);

  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Open confirmation
      </button>
      <ConfirmDialog
        open={open}
        title="Save configuration"
        confirmLabel="Save"
        busy={busy}
        onConfirm={() => {
          setBusy(true);
          toast.success("Saved");
        }}
        onCancel={() => setOpen(false)}
      >
        Save the pending changes.
      </ConfirmDialog>
    </>
  );
}

afterEach(() => {
  document.body.innerHTML = "";
});

describe("ToastProvider during confirmation", () => {
  it("keeps a confirmation toast announced but makes its dismiss action inert", async () => {
    render(
      <ToastProvider>
        <ToastConfirmationLifecycle />
      </ToastProvider>,
    );

    fireEvent.click(screen.getByRole("button", { name: "Open confirmation" }));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    const dismiss = screen.getByRole("button", { name: "Dismiss" });
    await waitFor(() => expect(dismiss.hasAttribute("inert")).toBe(true));
    expect(document.querySelector("[data-toast-container]")?.getAttribute("aria-hidden")).not.toBe("true");
    expect(screen.getByRole("dialog").contains(document.activeElement)).toBe(true);
  });
});
