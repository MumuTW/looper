import { useEffect, useId, useRef, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { Button } from "@/components/ui/button";

export type ConfirmDialogProps = {
  open: boolean;
  title: string;
  /** Dense body: string or custom nodes (e.g. takeover copy fields). */
  children: ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  /** When false, only the confirm/close button is shown (e.g. result dialogs). */
  showCancel?: boolean;
  /** danger styling for destructive actions */
  danger?: boolean;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
};

const focusableSelector = [
  "button:not([disabled])",
  "a[href]",
  "input:not([disabled])",
  "select:not([disabled])",
  "textarea:not([disabled])",
  '[tabindex]:not([tabindex="-1"])',
].join(",");

function focusableElements(panel: HTMLElement): HTMLElement[] {
  return Array.from(panel.querySelectorAll<HTMLElement>(focusableSelector)).filter(
    (element) =>
      !element.hidden &&
      element.getAttribute("aria-hidden") !== "true" &&
      element.tabIndex >= 0,
  );
}

const activeModalsStack: HTMLElement[] = [];
const originalBodyChildStates = new Map<
  Element,
  { inert: string | null; ariaHidden: string | null }
>();
let bodyObserver: MutationObserver | null = null;

function updateBackgroundInertState() {
  if (typeof document === "undefined") return;

  const topModal = activeModalsStack[activeModalsStack.length - 1];

  for (const child of Array.from(document.body.children)) {
    if (
      child.hasAttribute("data-toast-container") ||
      child.getAttribute("aria-live") ||
      child.querySelector("[aria-live]")
    ) {
      child.removeAttribute("inert");
      child.removeAttribute("aria-hidden");
      continue;
    }

    if (activeModalsStack.includes(child as HTMLElement)) {
      if (child === topModal) {
        child.removeAttribute("inert");
        child.removeAttribute("aria-hidden");
      } else {
        child.setAttribute("inert", "");
        child.setAttribute("aria-hidden", "true");
      }
      continue;
    }

    if (!originalBodyChildStates.has(child)) {
      originalBodyChildStates.set(child, {
        inert: child.getAttribute("inert"),
        ariaHidden: child.getAttribute("aria-hidden"),
      });
    }

    child.setAttribute("inert", "");
    child.setAttribute("aria-hidden", "true");
  }
}

function hideBackground(modalRoot: HTMLElement): () => void {
  activeModalsStack.push(modalRoot);
  updateBackgroundInertState();

  if (!bodyObserver && typeof document !== "undefined") {
    bodyObserver = new MutationObserver(() => {
      updateBackgroundInertState();
    });
    bodyObserver.observe(document.body, { childList: true });
  }

  return () => {
    const idx = activeModalsStack.indexOf(modalRoot);
    if (idx !== -1) {
      activeModalsStack.splice(idx, 1);
    }

    if (activeModalsStack.length > 0) {
      updateBackgroundInertState();
    } else {
      if (bodyObserver) {
        bodyObserver.disconnect();
        bodyObserver = null;
      }
      for (const [child, state] of originalBodyChildStates.entries()) {
        if (child.isConnected) {
          if (state.inert === null) child.removeAttribute("inert");
          else child.setAttribute("inert", state.inert);
          if (state.ariaHidden === null) child.removeAttribute("aria-hidden");
          else child.setAttribute("aria-hidden", state.ariaHidden);
        }
      }
      originalBodyChildStates.clear();
    }
  };
}

/**
 * Dense operator confirmation modal.
 *
 * Safe initial focus prefers Cancel, then Confirm when no cancel action exists,
 * and finally the panel when every action is disabled. Closing restores the
 * initiating element if it still belongs to the document.
 */
export function ConfirmDialog({
  open,
  title,
  children,
  confirmLabel = "Confirm",
  cancelLabel = "Cancel",
  showCancel = true,
  danger = false,
  busy = false,
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  const titleId = useId();
  const modalRootRef = useRef<HTMLDivElement | null>(null);
  const panelRef = useRef<HTMLDivElement | null>(null);
  const initiatingElementRef = useRef<HTMLElement | null>(null);

  useEffect(() => {
    if (!open) return;
    initiatingElementRef.current =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null;

    const restoreBackground = modalRootRef.current
      ? hideBackground(modalRootRef.current)
      : () => undefined;
    const cancelButton = panelRef.current?.querySelector<HTMLButtonElement>(
      "[data-confirm-dialog-cancel]",
    );
    const confirmButton = panelRef.current?.querySelector<HTMLButtonElement>(
      "[data-confirm-dialog-confirm]",
    );
    const initialFocus =
      (showCancel && !busy ? cancelButton : null) ??
      (!busy ? confirmButton : null) ??
      panelRef.current;
    initialFocus?.focus();

    return () => {
      restoreBackground();
      const initiatingElement = initiatingElementRef.current;
      initiatingElementRef.current = null;
      if (activeModalsStack.length === 0 && initiatingElement?.isConnected) {
        initiatingElement.focus();
      }
    };
  }, [open]);

  useEffect(() => {
    if (!open || !busy || !panelRef.current) return;
    const active = document.activeElement;
    if (
      !active ||
      !panelRef.current.contains(active) ||
      (active as HTMLButtonElement).disabled
    ) {
      panelRef.current.focus();
    }
  }, [open, busy]);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !busy) {
        e.preventDefault();
        onCancel();
        return;
      }
      if (e.key !== "Tab" || !panelRef.current) return;

      const focusable = focusableElements(panelRef.current);
      if (focusable.length === 0) {
        e.preventDefault();
        panelRef.current.focus();
        return;
      }
      const activeIndex = focusable.indexOf(
        document.activeElement as HTMLElement,
      );
      if (e.shiftKey && activeIndex <= 0) {
        e.preventDefault();
        focusable[focusable.length - 1]?.focus();
      } else if (!e.shiftKey && activeIndex === focusable.length - 1) {
        e.preventDefault();
        focusable[0]?.focus();
      } else if (activeIndex === -1) {
        e.preventDefault();
        (e.shiftKey ? focusable[focusable.length - 1] : focusable[0])?.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open, busy, onCancel]);

  if (!open) return null;
  if (typeof document === "undefined") return null;

  return createPortal(
    <div
      ref={modalRootRef}
      className="fixed inset-0 z-40 flex items-center justify-center p-3"
    >
      <button
        type="button"
        className="absolute inset-0 border-0 bg-black/40 p-0"
        aria-hidden="true"
        tabIndex={-1}
        disabled={busy}
        onClick={() => {
          if (!busy) onCancel();
        }}
      />
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        className="relative z-10 w-full max-w-md rounded border border-[var(--border)] bg-[var(--bg-elevated)] shadow-lg outline-none"
      >
        <header className="border-b border-[var(--border)] px-3 py-2">
          <h2 id={titleId} className="m-0 text-[13px] font-semibold">
            {title}
          </h2>
        </header>
        <div className="px-3 py-2 text-[12px] text-[var(--text)]">
          {children}
        </div>
        <footer className="flex justify-end gap-1.5 border-t border-[var(--border)] px-3 py-2">
          {showCancel ? (
            <Button
              data-confirm-dialog-cancel
              variant="ghost"
              size="sm"
              onClick={onCancel}
              disabled={busy}
            >
              {cancelLabel}
            </Button>
          ) : null}
          <Button
            data-confirm-dialog-confirm
            variant={danger ? "danger" : "default"}
            size="sm"
            onClick={onConfirm}
            disabled={busy}
          >
            {busy ? "…" : confirmLabel}
          </Button>
        </footer>
      </div>
    </div>,
    document.body,
  );
}
