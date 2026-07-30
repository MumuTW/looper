import { act, cleanup, render, screen } from "@testing-library/react";
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

import { LogsPane } from "@/pages/LoopDetail";

const encoder = new TextEncoder();

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

function snapshot(stdout: string): string {
  return `event: snapshot\ndata: ${JSON.stringify({
    loop: { id: "loop_1", seq: 1, status: "running" },
    run: { runId: "run_1", status: "running" },
    agent: { executionId: "exec_1", status: "running", stdout, stderr: "" },
  })}\n\n`;
}

beforeEach(() => {
  vi.useFakeTimers();
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
  vi.unstubAllGlobals();
});

describe("LogsPane stream recovery", () => {
  it("shows degraded after a typed mid-stream error and returns live after reconnect", async () => {
    let primaryAttempts = 0;
    apiMocks.openLoopLogsStream.mockImplementation(
      async (_selector: string, _signal: AbortSignal, opts?: { stderr?: boolean }) => {
        if (opts?.stderr) return hangingSSE(snapshot(""));
        primaryAttempts += 1;
        if (primaryAttempts === 1) {
          return finiteSSE(
            snapshot("before failure\n") +
              `event: error\ndata: ${JSON.stringify({
                code: "INTERNAL_ERROR",
                message: "database is locked",
                retryable: true,
                retryAfterMs: 1000,
              })}\n\n`,
          );
        }
        return hangingSSE(snapshot("after recovery\n"));
      },
    );

    render(<LogsPane selector="loop_1" />);

    await vi.waitFor(() => {
      expect(screen.getByText("degraded")).toBeTruthy();
    });
    expect(screen.getByText("database is locked")).toBeTruthy();
    expect(screen.getByText(/before failure/)).toBeTruthy();

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

  it("degrades and reconnects when the stderr follow fails", async () => {
    let primaryAttempts = 0;
    let stderrAttempts = 0;
    apiMocks.openLoopLogsStream.mockImplementation(
      async (
        _selector: string,
        _signal: AbortSignal,
        opts?: { stderr?: boolean },
      ) => {
        if (opts?.stderr) {
          stderrAttempts += 1;
          if (stderrAttempts === 1) {
            return finiteSSE(
              snapshot("") +
                `event: error\ndata: ${JSON.stringify({
                  code: "INTERNAL_ERROR",
                  message: "stderr read failed",
                  retryable: true,
                  retryAfterMs: 1000,
                })}\n\n`,
            );
          }
          return hangingSSE(snapshot(""));
        }

        primaryAttempts += 1;
        return hangingSSE(
          snapshot(
            primaryAttempts === 1 ? "before stderr failure\n" : "recovered\n",
          ),
        );
      },
    );

    render(<LogsPane selector="loop_1" />);

    await vi.waitFor(() => {
      expect(screen.getByText("degraded")).toBeTruthy();
    });
    expect(screen.getByText("stderr read failed")).toBeTruthy();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });

    await vi.waitFor(() => {
      expect(screen.getByText("live")).toBeTruthy();
    });
    expect(screen.queryByText("stderr read failed")).toBeNull();
    expect(screen.getByText(/recovered/)).toBeTruthy();
    expect(primaryAttempts).toBe(2);
    expect(stderrAttempts).toBe(2);
  });
});
