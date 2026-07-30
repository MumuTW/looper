import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { LoopActionBar } from "@/components/LoopActionBar";
import { ToastProvider } from "@/lib/toast";

const takeoverLoop = vi.fn();

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return {
    ...actual,
    takeoverLoop: (...args: unknown[]) => takeoverLoop(...args),
  };
});

function deferred<T>() {
  let resolve: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function renderTakeover(onMutated: () => Promise<void>) {
  render(
    <ToastProvider>
      <LoopActionBar
        selector="3491"
        status="running"
        hasActiveRun
        mode="full"
        onMutated={onMutated}
      />
    </ToastProvider>,
  );
}

beforeEach(() => {
  takeoverLoop.mockReset();
  takeoverLoop.mockResolvedValue({
    supported: true,
    worktreePath: "/tmp/loop-3491",
    resumeCommand: "codex resume",
  });
});

afterEach(() => cleanup());

describe("LoopActionBar confirmation lifecycle", () => {
  it("keeps focus in the confirmation while an asynchronous takeover is busy", async () => {
    const request = deferred<{
      supported: boolean;
      worktreePath: string;
      resumeCommand: string;
    }>();
    const mutation = deferred<void>();
    takeoverLoop.mockReturnValueOnce(request.promise);
    renderTakeover(() => mutation.promise);

    fireEvent.click(screen.getByRole("button", { name: "Takeover" }));
    await screen.findByRole("dialog", { name: "Take over loop?" });
    fireEvent.click(screen.getByRole("button", { name: "Takeover" }));

    await waitFor(() => {
      expect(
        screen
          .getByRole("dialog", { name: "Take over loop?" })
          .querySelector<HTMLButtonElement>("[data-confirm-dialog-confirm]")
          ?.disabled,
      ).toBe(true);
    });
    expect(
      screen
        .getByRole("dialog", { name: "Take over loop?" })
        .contains(document.activeElement),
    ).toBe(true);

    await act(async () => {
      request.resolve({
        supported: true,
        worktreePath: "/tmp/loop-3491",
        resumeCommand: "codex resume",
      });
      mutation.resolve(undefined);
    });
  });

  it("restores the original Takeover control after the stacked result dialog closes", async () => {
    const mutation = deferred<void>();
    renderTakeover(() => mutation.promise);

    const trigger = screen.getByRole("button", { name: "Takeover" });
    trigger.focus();
    fireEvent.click(trigger);
    await screen.findByRole("dialog", { name: "Take over loop?" });
    fireEvent.click(screen.getByRole("button", { name: "Takeover" }));
    await screen.findByRole("dialog", { name: "Takeover result" });

    await act(async () => mutation.resolve(undefined));
    await waitFor(() => {
      expect(screen.getAllByRole("dialog")).toHaveLength(1);
    });
    fireEvent.click(screen.getByRole("button", { name: "Close" }));

    expect(document.activeElement).toBe(trigger);
  });
});
