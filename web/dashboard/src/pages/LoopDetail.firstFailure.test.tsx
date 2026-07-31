import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { Loop } from "@/lib/api";
import { DashboardDataProvider } from "@/lib/DashboardDataContext";
import { ToastProvider } from "@/lib/toast";
import { LoopDetailPage } from "./LoopDetail";

function loopFixture(overrides: Partial<Loop> = {}): Loop {
  return {
    id: "loop_1",
    seq: 42,
    projectId: "project_1",
    type: "fixer",
    targetType: "pull_request",
    targetId: "pr:acme/looper:42",
    repo: "acme/looper",
    prNumber: 42,
    status: "failed",
    lastFailureKind: "retryable_transient",
    lastFailureReason: "queue reported the most recent error",
    createdAt: "2026-07-30T12:00:00.000Z",
    updatedAt: "2026-07-30T12:00:00.000Z",
    ...overrides,
  };
}

function renderLoopDetail(loop: Loop) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      const data = url.includes("/loops/") ? loop : { items: [] };
      // apiFetch requires a well-formed success envelope.
      return new Response(JSON.stringify({ ok: true, data }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    }),
  );
  return render(
    <MemoryRouter initialEntries={["/loops/42"]}>
      <ToastProvider>
        <DashboardDataProvider>
          <Routes>
            <Route path="/loops/:selector" element={<LoopDetailPage />} />
          </Routes>
        </DashboardDataProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("LoopDetail first-failure row", () => {
  it("shows the causal failure alongside the queue's latest error", async () => {
    renderLoopDetail(
      loopFixture({
        outcome: {
          primaryFailure: { step: "repair", message: "agent timed out" },
          secondaryIssues: [{ step: "push", message: "remote moved" }],
        },
      }),
    );

    await waitFor(() => {
      expect(screen.getByText("First failure")).toBeTruthy();
    });
    // The point of the row: it differs from lastFailureReason.
    expect(
      screen.getByText("repair — agent timed out (+1 later issue)"),
    ).toBeTruthy();
    expect(
      screen.getByText("queue reported the most recent error"),
    ).toBeTruthy();
  });

  it("falls back to a dash when the run recorded no outcome", async () => {
    renderLoopDetail(loopFixture());

    await waitFor(() => {
      expect(screen.getByText("First failure")).toBeTruthy();
    });
    const row = screen.getByText("First failure").closest("div");
    expect(row?.textContent).toContain("—");
  });
});

describe("LoopDetail durable progress row", () => {
  it("labels a failed run that still shipped something as partial", async () => {
    renderLoopDetail(
      loopFixture({
        outcome: {
          primaryFailure: { step: "resolve_comments", message: "resolve failed" },
          progress: { pushed: true, repliesSent: 2 },
          partialSuccess: true,
        },
      }),
    );

    await waitFor(() => {
      expect(screen.getByText("Kept (partial)")).toBeTruthy();
    });
    expect(screen.getByText("pushed · 2 replies")).toBeTruthy();
  });

  it("shows a dash and no partial label when the run achieved nothing", async () => {
    renderLoopDetail(
      loopFixture({
        outcome: {
          primaryFailure: { step: "repair", message: "agent timed out" },
          progress: {},
        },
      }),
    );

    await waitFor(() => {
      expect(screen.getByText("Kept")).toBeTruthy();
    });
    expect(screen.queryByText("Kept (partial)")).toBeNull();
  });
});

describe("LoopDetail secondary issues row", () => {
  it("lists issues around the run's own result, separately from the cause", async () => {
    renderLoopDetail(
      loopFixture({
        outcome: {
          primaryFailure: { step: "repair", message: "agent timed out" },
          secondaryIssues: [
            { step: "recheck", message: "worktree cleanup refused: dirty" },
          ],
        },
      }),
    );

    await waitFor(() => {
      expect(screen.getByText("Also")).toBeTruthy();
    });
    expect(
      screen.getByText("recheck — worktree cleanup refused: dirty"),
    ).toBeTruthy();
    // The causal failure stays in its own row rather than being folded in.
    expect(
      screen.getByText("repair — agent timed out (+1 later issue)"),
    ).toBeTruthy();
  });

  it("shows a dash when nothing else happened", async () => {
    renderLoopDetail(
      loopFixture({
        outcome: { primaryFailure: { step: "repair", message: "agent timed out" } },
      }),
    );

    await waitFor(() => {
      expect(screen.getByText("Also")).toBeTruthy();
    });
    const row = screen.getByText("Also").closest("div");
    expect(row?.textContent).toContain("—");
  });
});

describe("LoopDetail reviewer convergence", () => {
  it("shows the durable round budget, open items, and productivity trend", async () => {
    renderLoopDetail(
      loopFixture({
        type: "reviewer",
        convergence: {
          policy: {
            maxConsecutiveUnproductive: 3,
            maxFixerAttemptsPerItem: 4,
            maxTotalRounds: 40,
            severityFloor: "non_blocking",
          },
          state: {
            totalRounds: 5,
            consecutiveUnproductive: 2,
            items: {
              "review-1": {
                id: "review-1",
                severity: "blocking",
                status: "open",
                fixerAttempts: 1,
              },
              "review-2": {
                id: "review-2",
                severity: "non_blocking",
                status: "resolved",
              },
            },
            history: [
              { number: 4, productive: true, openItemIds: ["review-1"] },
              { number: 5, productive: false, openItemIds: ["review-1"] },
            ],
          },
          action: "escalate",
          reason: "stalled",
          status: "awaiting_human",
          updatedAt: "2026-07-31T18:00:00.000Z",
        },
      }),
    );

    await waitFor(() => {
      expect(screen.getByText("Reviewer convergence")).toBeTruthy();
    });
    expect(screen.getByText("5 / 40")).toBeTruthy();
    expect(screen.getByText("2 / 3")).toBeTruthy();
    expect(screen.getByText("review-1")).toBeTruthy();
    expect(screen.getByText("#4 productive")).toBeTruthy();
    expect(screen.getByText("#5 unproductive")).toBeTruthy();
  });
});
