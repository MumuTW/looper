import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const apiMocks = vi.hoisted(() => ({
  openLoopLogsStream: vi.fn(),
}));

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return {
    ...actual,
    openLoopLogsStream: (...args: unknown[]) =>
      apiMocks.openLoopLogsStream(...args),
  };
});

import { LoopDetailPage } from "@/pages/LoopDetail";
import { DashboardDataProvider } from "@/lib/DashboardDataContext";
import { ProjectFilterProvider } from "@/lib/ProjectFilterContext";
import { ToastProvider } from "@/lib/toast";
import type { Loop, LoopLogsSnapshot } from "@/lib/api";

const encoder = new TextEncoder();

function response(data: unknown, status = 200): Response {
  return new Response(
    JSON.stringify(
      status >= 400 ? { ok: false, error: data } : { ok: true, data, error: null },
    ),
    { status, headers: { "Content-Type": "application/json" } },
  );
}

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
    attempts: 2,
    maxAttempts: 5,
    createdAt: "2026-07-30T12:00:00.000Z",
    updatedAt: "2026-07-30T12:00:00.000Z",
    outcome: {
      primaryFailure: { step: "repair", message: "agent timed out" },
      secondaryIssues: [{ step: "push", message: "remote moved" }],
      progress: { pushed: true, repliesSent: 2 },
      partialSuccess: true,
    },
    ...overrides,
  };
}

function finiteSSE(text: string): Response {
  return new Response(
    new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(encoder.encode(text));
        controller.close();
      },
    }),
    { status: 200, headers: { "Content-Type": "text/event-stream" } },
  );
}

function hangingSSE(text: string): Response {
  return new Response(
    new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(encoder.encode(text));
      },
      pull() {
        return new Promise(() => {});
      },
    }),
    { status: 200, headers: { "Content-Type": "text/event-stream" } },
  );
}

function snapshotSSE(stdout: string, stderr = ""): string {
  const snap: LoopLogsSnapshot = {
    seq: 42,
    loopId: "loop_1",
    loopType: "fixer",
    loopStatus: "failed",
    run: { runId: "run_1", status: "failed", startedAt: "2026-07-30T12:00:00.000Z" },
    agent: {
      executionId: "exec_1",
      vendor: "codex",
      status: "failed",
      pid: 1234,
      startedAt: "2026-07-30T12:00:00.000Z",
      heartbeatCount: 1,
      stdout,
      stderr,
    },
  };
  return `event: snapshot\ndata: ${JSON.stringify(snap)}\n\n`;
}

function chunkSSE(content: string): string {
  return `event: chunk\ndata: ${JSON.stringify({ stream: "stdout", content })}\n\n`;
}

function errorSSE(message: string, retryAfterMs = 1000): string {
  return `event: error\ndata: ${JSON.stringify({
    code: "INTERNAL_ERROR",
    message,
    retryable: true,
    retryAfterMs,
  })}\n\n`;
}

const endSSE = "event: end\ndata: {}\n\n";

type StreamOpts = { stderr?: boolean };

/**
 * Default dual-SSE mock: a fresh stream instance per call (a Response body can
 * only be read once), with the stderr follow staying open so it never races the
 * primary stream's terminal state. needsSeparateStderrFollow() always opens a
 * stderr follow, so every test must serve both streams.
 *
 * openLoopLogsStream is called as (selector, signal, opts) — the stderr flag
 * lives on the third argument, not the first.
 */
function defaultStreams(
  primary: string,
  stderr = snapshotSSE(""),
): (
  selector: string,
  signal: AbortSignal,
  opts?: StreamOpts,
) => Promise<Response> {
  return async (_selector: string, _signal: AbortSignal, opts?: StreamOpts) =>
    opts?.stderr ? hangingSSE(stderr) : finiteSSE(primary);
}

function stubFetch(loop: Loop | (() => Response) = loopFixture()): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url === "/api/v1/healthz") return response({ healthy: true });
    if (url === "/api/v1/projects") return response({ items: [] });
    if (url === "/api/v1/runs/active") return response({ items: [] });
    if (url.includes("/loops/") && !url.includes("/logs")) {
      return typeof loop === "function" ? loop() : response(loop);
    }
    return response({ items: [] });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function renderLoopDetail(initialPath = "/loops/42") {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <ToastProvider>
        <DashboardDataProvider>
          <ProjectFilterProvider>
            <Routes>
              <Route path="/loops" element={<div data-testid="loops-index">loops</div>} />
              <Route path="/loops/:selector" element={<LoopDetailPage />} />
            </Routes>
          </ProjectFilterProvider>
        </DashboardDataProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  apiMocks.openLoopLogsStream.mockReset();
  vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
    callback(0);
    return 1;
  });
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    value: "visible",
  });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
  window.history.replaceState({}, "", "/");
});

// SSE recovery and post-mutation refresh flows involve timers and confirm
// dialogs that take seconds on a loaded CI machine; keep a generous backstop
// local to this suite instead of weakening every dashboard test.
describe("LoopDetailPage", { timeout: 30_000 }, () => {
  it("reports a missing selector instead of mounting the logs pane", () => {
    render(
      <MemoryRouter initialEntries={["/loops"]}>
        <ToastProvider>
          <DashboardDataProvider>
            <ProjectFilterProvider>
              <Routes>
                <Route path="/loops" element={<LoopDetailPage />} />
              </Routes>
            </ProjectFilterProvider>
          </DashboardDataProvider>
        </ToastProvider>
      </MemoryRouter>,
    );

    expect(screen.getByText("Missing loop selector")).toBeTruthy();
    expect(apiMocks.openLoopLogsStream).not.toHaveBeenCalled();
  });

  it("shows a loading state before the first loop metadata response", async () => {
    stubFetch(() => new Promise<Response>(() => {}));
    apiMocks.openLoopLogsStream.mockImplementation(defaultStreams(snapshotSSE("")));

    renderLoopDetail();

    expect(await screen.findByText("Loading loop…")).toBeTruthy();
  });

  it("surfaces a metadata error with a retry affordance", async () => {
    let calls = 0;
    stubFetch(() => {
      calls += 1;
      return calls === 1
        ? response({ message: "loop not found" }, 404)
        : response(loopFixture());
    });
    apiMocks.openLoopLogsStream.mockImplementation(defaultStreams(snapshotSSE("")));

    renderLoopDetail();

    expect(await screen.findByText("loop not found")).toBeTruthy();
    // Only the Metadata card exposes Retry (the logs pane stays open).
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => {
      expect(screen.getByText("First failure")).toBeTruthy();
    });
  });

  it("renders metadata authority fields, the back link, and the logs pane together", async () => {
    stubFetch();
    apiMocks.openLoopLogsStream.mockImplementation(
      defaultStreams(
        snapshotSSE("agent stdout seed\n") + chunkSSE("appended chunk\n") + endSSE,
      ),
    );

    renderLoopDetail();

    await waitFor(() => {
      expect(screen.getByText("First failure")).toBeTruthy();
    });
    // Header shows the loop seq; the back link returns to the loops list.
    expect(screen.getByRole("heading", { name: /#42/ })).toBeTruthy();
    expect(screen.getByRole("link", { name: "← Loops" })).toBeTruthy();
    // Metadata authority fields.
    expect(screen.getByText("project_1")).toBeTruthy();
    expect(screen.getByText("acme/looper")).toBeTruthy();
    expect(screen.getByText("retryable_transient")).toBeTruthy();
    expect(
      screen.getByText("queue reported the most recent error"),
    ).toBeTruthy();
    // Outcome projection rows.
    expect(
      screen.getByText("repair — agent timed out (+1 later issue)"),
    ).toBeTruthy();
    expect(screen.getByText("Kept (partial)")).toBeTruthy();
    expect(screen.getByText("pushed · 2 replies")).toBeTruthy();
    expect(screen.getByText("push — remote moved")).toBeTruthy();
    // Logs pane is wired to the route selector and shows the streamed content.
    expect(screen.getByText(/agent stdout seed/)).toBeTruthy();
    expect(screen.getByText(/appended chunk/)).toBeTruthy();
  });

  it("presents a terminal status when the stdout stream ends normally", async () => {
    stubFetch();
    // Both primary and stderr follow end cleanly → the pane reaches "ended".
    apiMocks.openLoopLogsStream.mockImplementation(
      async (_selector: string, _signal: AbortSignal, opts?: StreamOpts) =>
        opts?.stderr
          ? finiteSSE(snapshotSSE("") + endSSE)
          : finiteSSE(snapshotSSE("final stdout\n") + endSSE),
    );

    renderLoopDetail();

    await waitFor(() => {
      expect(screen.getByText("ended")).toBeTruthy();
    });
    expect(screen.getByText(/final stdout/)).toBeTruthy();
  });

  it("degrades on a mid-stream error and returns live after reconnect", async () => {
    vi.useFakeTimers();
    stubFetch();
    let primaryAttempts = 0;
    apiMocks.openLoopLogsStream.mockImplementation(
      async (_selector: string, _signal: AbortSignal, opts?: StreamOpts) => {
        if (opts?.stderr) return hangingSSE(snapshotSSE(""));
        primaryAttempts += 1;
        if (primaryAttempts === 1) {
          return finiteSSE(snapshotSSE("before failure\n") + errorSSE("database is locked", 1000));
        }
        return hangingSSE(snapshotSSE("after recovery\n"));
      },
    );

    renderLoopDetail();

    await vi.waitFor(() => {
      expect(screen.getByText("degraded")).toBeTruthy();
    });
    expect(screen.getByText("database is locked")).toBeTruthy();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });

    await vi.waitFor(() => {
      expect(screen.getByText("live")).toBeTruthy();
    });
    expect(screen.queryByText("database is locked")).toBeNull();
    expect(screen.getByText(/after recovery/)).toBeTruthy();
    expect(primaryAttempts).toBe(2);
  });

  it("refreshes metadata and active runs after a destructive stop mutation", async () => {
    let stopped = false;
    const activeRun = {
      seq: 42,
      runId: "run_1",
      loopId: "loop_1",
      projectId: "project_1",
      type: "fixer",
      status: "running",
      loopStatus: "running",
      displayStatus: "running",
      target: {
        type: "pull_request",
        projectId: "project_1",
        repo: "acme/looper",
        prNumber: 42,
        label: "acme/looper#42",
      },
      agent: {
        active: true,
        activeCount: 1,
        executionId: "exec_1",
        vendor: "codex",
        pid: 1234,
        startedAt: "2026-07-30T12:00:00.000Z",
        heartbeatCount: 1,
        status: "running",
      },
      startedAt: "2026-07-30T12:00:00.000Z",
    };
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url === "/api/v1/healthz") return response({ healthy: true });
      if (url === "/api/v1/projects") return response({ items: [] });
      if (url === "/api/v1/runs/active") {
        return response(stopped ? { items: [] } : { items: [activeRun] });
      }
      if (url.includes("/runs/active/") && url.endsWith("/stop") && init?.method === "POST") {
        stopped = true;
        return response({ stopped: true, loopId: "loop_1" });
      }
      if (url.includes("/loops/") && !url.includes("/logs")) {
        return response(loopFixture({ status: stopped ? "paused" : "running" }));
      }
      return response({ items: [] });
    });
    vi.stubGlobal("fetch", fetchMock);
    apiMocks.openLoopLogsStream.mockImplementation(defaultStreams(snapshotSSE("")));

    renderLoopDetail();

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
    });

    fireEvent.click(screen.getByRole("button", { name: "Stop" }));
    await screen.findByRole("dialog");
    // The dialog confirm is the second "Stop" affordance in the DOM.
    fireEvent.click(screen.getAllByRole("button", { name: "Stop" }).pop()!);

    await waitFor(() => {
      expect(screen.getAllByText("paused").length).toBeGreaterThan(0);
    });
    expect(
      fetchMock.mock.calls.some(([input]) => String(input) === "/api/v1/runs/active"),
    ).toBe(true);
  });
});
