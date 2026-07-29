import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  classifyRetryWorktree,
  isWorktreeRouteUnavailable,
  LoopActionBar,
} from "@/components/LoopActionBar";
import { ApiError } from "@/lib/api";
import { ToastProvider } from "@/lib/toast";

const fetchLoopWorktree = vi.fn();
const retryLoop = vi.fn();
const pauseLoop = vi.fn();
const startLoop = vi.fn();
const stopActiveRun = vi.fn();
const takeoverLoop = vi.fn();
const handbackLoop = vi.fn();

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return {
    ...actual,
    fetchLoopWorktree: (...args: unknown[]) => fetchLoopWorktree(...args),
    retryLoop: (...args: unknown[]) => retryLoop(...args),
    pauseLoop: (...args: unknown[]) => pauseLoop(...args),
    startLoop: (...args: unknown[]) => startLoop(...args),
    stopActiveRun: (...args: unknown[]) => stopActiveRun(...args),
    takeoverLoop: (...args: unknown[]) => takeoverLoop(...args),
    handbackLoop: (...args: unknown[]) => handbackLoop(...args),
  };
});

function renderBar(props?: Partial<React.ComponentProps<typeof LoopActionBar>>) {
  const onMutated = vi.fn().mockResolvedValue(undefined);
  render(
    <ToastProvider>
      <LoopActionBar
        selector="3491"
        status="paused"
        mode="full"
        onMutated={onMutated}
        {...props}
      />
    </ToastProvider>,
  );
  return { onMutated };
}

describe("classifyRetryWorktree", () => {
  it("offers discard only for present managed dirty trees", () => {
    expect(
      classifyRetryWorktree({
        loopId: "l",
        seq: 1,
        present: true,
        managed: true,
        dirty: true,
      }),
    ).toBe("offer-discard");
    expect(
      classifyRetryWorktree({
        loopId: "l",
        seq: 1,
        present: true,
        managed: false,
        dirty: true,
      }),
    ).toBe("inspect-only");
    expect(
      classifyRetryWorktree({
        loopId: "l",
        seq: 1,
        present: true,
        managed: true,
        dirty: false,
      }),
    ).toBe("ok");
    expect(
      classifyRetryWorktree({
        loopId: "l",
        seq: 1,
        present: false,
        managed: true,
        dirty: true,
      }),
    ).toBe("ok");
  });
});

describe("isWorktreeRouteUnavailable", () => {
  it("detects older-daemon route missing", () => {
    expect(
      isWorktreeRouteUnavailable(
        new ApiError("Unknown route", {
          status: 404,
          code: "ROUTE_NOT_FOUND",
        }),
      ),
    ).toBe(true);
    expect(
      isWorktreeRouteUnavailable(
        new ApiError("boom", { status: 500, code: "INTERNAL_ERROR" }),
      ),
    ).toBe(false);
  });

  // These are answers from the route itself. Treating them as "route missing"
  // skips the dirty-worktree gate and retries with unreviewed edits in the tree.
  it("holds the gate for a 404 that is not ROUTE_NOT_FOUND", () => {
    expect(
      isWorktreeRouteUnavailable(
        new ApiError("Project not found: acme", {
          status: 404,
          code: "PROJECT_NOT_FOUND",
        }),
      ),
    ).toBe(false);
  });

  it("holds the gate for a non-404 whose message mentions 404", () => {
    expect(
      isWorktreeRouteUnavailable(
        new ApiError(
          "Failed to stat worktree at /w/looper-fix-acme-pr-404: permission denied",
          { status: 500, code: "INTERNAL_ERROR" },
        ),
      ),
    ).toBe(false);
  });
});

describe("LoopActionBar retry dirty UX", () => {
  beforeEach(() => {
    fetchLoopWorktree.mockReset();
    retryLoop.mockReset();
    pauseLoop.mockReset();
    startLoop.mockReset();
    stopActiveRun.mockReset();
    takeoverLoop.mockReset();
    handbackLoop.mockReset();
    retryLoop.mockResolvedValue({
      loop: { id: "loop_1", status: "queued" },
      mode: "auto",
      resetAttempts: true,
      discardWorktreeChanges: false,
    });
  });

  afterEach(() => {
    cleanup();
  });

  it("retries immediately when worktree is clean", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 3491,
      present: true,
      managed: true,
      dirty: false,
      clean: true,
      worktreePath: "/tmp/wt",
    });
    const { onMutated } = renderBar();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => {
      expect(retryLoop).toHaveBeenCalledWith("3491", {
        discardWorktreeChanges: false,
      });
    });
    expect(onMutated).toHaveBeenCalled();
    expect(screen.queryByText(/Dirty worktree/i)).toBeNull();
  });

  it("confirms discard for managed dirty worktree and posts discard=true", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 3491,
      present: true,
      managed: true,
      dirty: true,
      clean: false,
      worktreePath: "/tmp/dirty-wt",
      branch: "feat/x",
    });
    renderBar();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await screen.findByText(/Dirty worktree — discard and retry/i);
    expect(retryLoop).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Discard & retry" }));

    await waitFor(() => {
      expect(retryLoop).toHaveBeenCalledWith("3491", {
        discardWorktreeChanges: true,
      });
    });
  });

  it("inspect-first cancels discard and shows the worktree cd command", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 3491,
      present: true,
      managed: true,
      dirty: true,
      clean: false,
      worktreePath: "/tmp/dirty-wt",
      branch: "feat/x",
    });
    renderBar();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await screen.findByText(/Dirty worktree — discard and retry/i);

    fireEvent.click(screen.getByRole("button", { name: "Inspect first" }));

    await screen.findByText(/Inspect dirty worktree/i);
    expect(screen.getByText("cd -- '/tmp/dirty-wt'")).toBeTruthy();
    expect(screen.getByText("/tmp/dirty-wt")).toBeTruthy();
    expect(
      screen.getByText(
        'POST /api/v1/loops/3491/retry {"discardWorktreeChanges": true}',
      ),
    ).toBeTruthy();
    expect(retryLoop).not.toHaveBeenCalled();
  });

  it("shell-quotes worktree paths that contain spaces", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 3491,
      present: true,
      managed: true,
      dirty: true,
      clean: false,
      worktreePath: "/Users/Alice Smith/worktrees/x",
      branch: "feat/x",
    });
    renderBar();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await screen.findByText(/Dirty worktree — discard and retry/i);
    fireEvent.click(screen.getByRole("button", { name: "Inspect first" }));

    await screen.findByText(/Inspect dirty worktree/i);
    expect(
      screen.getByText("cd -- '/Users/Alice Smith/worktrees/x'"),
    ).toBeTruthy();
  });

  it("does not offer discard for unmanaged dirty worktree", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 3491,
      present: true,
      managed: false,
      dirty: true,
      clean: false,
      worktreePath: "/tmp/primary-repo",
      reason: "unmanaged",
    });
    renderBar();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await screen.findByText(/Unmanaged dirty worktree/i);
    expect(screen.queryByText("Discard & retry")).toBeNull();
    expect(retryLoop).not.toHaveBeenCalled();
  });

  it("falls back to plain retry when /worktree route is missing", async () => {
    fetchLoopWorktree.mockRejectedValue(
      new ApiError("Unknown route", {
        status: 404,
        code: "ROUTE_NOT_FOUND",
      }),
    );
    renderBar();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => {
      expect(retryLoop).toHaveBeenCalledWith("3491", {
        discardWorktreeChanges: false,
      });
    });
  });

  // The route exists and answered: the project row is archived or gone. Its
  // message contains "not found", so a substring match reads it as a missing
  // route, skips the gate, and requeues the loop with unreviewed local edits.
  it("holds the gate when /worktree answers 404 PROJECT_NOT_FOUND", async () => {
    fetchLoopWorktree.mockRejectedValue(
      new ApiError("Project not found: acme", {
        status: 404,
        code: "PROJECT_NOT_FOUND",
      }),
    );
    renderBar();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => {
      expect(
        screen.getAllByText("Project not found: acme").length,
      ).toBeGreaterThan(0);
    });
    expect(retryLoop).not.toHaveBeenCalled();
  });

  it("surfaces non-404 preflight failures without retrying", async () => {
    fetchLoopWorktree.mockRejectedValue(
      new ApiError("git status failed", {
        status: 500,
        code: "INTERNAL_ERROR",
      }),
    );
    renderBar();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => {
      expect(screen.getAllByText("git status failed").length).toBeGreaterThan(0);
    });
    expect(retryLoop).not.toHaveBeenCalled();
  });
});
