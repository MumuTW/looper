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

function snapshot(stdout: string, stderr = ""): string {
  return `event: snapshot\ndata: ${JSON.stringify({
    loop: { id: "loop_1", seq: 1, status: "running" },
    run: { runId: "run_1", status: "running" },
    agent: { executionId: "exec_1", status: "running", stdout, stderr },
  })}\n\n`;
}

function chunk(stream: "stdout" | "stderr", content: string): string {
  return `event: chunk\ndata: ${JSON.stringify({ stream, content })}\n\n`;
}

const end = "event: end\ndata: {}\n\n";

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
  it("uses one connection for typed stdout and stderr chunks", async () => {
    apiMocks.openLoopLogsStream.mockResolvedValue(
      hangingSSE(
        snapshot("initial stdout\n", "initial stderr\n") +
          chunk("stdout", "more stdout\n") +
          chunk("stderr", "more stderr\n"),
      ),
    );

    render(<LogsPane selector="loop_1" />);

    await vi.waitFor(() => {
      expect(screen.getByText(/more stdout/)).toBeTruthy();
      expect(screen.getByText(/more stderr/)).toBeTruthy();
    });
    expect(screen.getByText("live")).toBeTruthy();
    expect(apiMocks.openLoopLogsStream).toHaveBeenCalledTimes(1);
    expect(screen.getByText(/--- stderr ---/)).toBeTruthy();
  });

  it("shows degraded after a typed mid-stream error and returns live after reconnect", async () => {
    let primaryAttempts = 0;
    apiMocks.openLoopLogsStream.mockImplementation(async () => {
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
    });

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

  it("reconnects after an unexpected close without an end event", async () => {
    let attempts = 0;
    apiMocks.openLoopLogsStream.mockImplementation(async () => {
      attempts += 1;
      if (attempts === 1) return finiteSSE(snapshot("before disconnect\n"));
      return hangingSSE(snapshot("after reconnect\n"));
    });

    render(<LogsPane selector="loop_1" />);

    await vi.waitFor(() => {
      expect(screen.getByText("connecting")).toBeTruthy();
    });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });

    await vi.waitFor(() => {
      expect(screen.getByText("live")).toBeTruthy();
    });
    expect(screen.getByText(/after reconnect/)).toBeTruthy();
    expect(attempts).toBe(2);
  });

  it("does not reconnect after the combined stream ends explicitly", async () => {
    apiMocks.openLoopLogsStream.mockResolvedValue(
      finiteSSE(
        snapshot("final stdout\n") + chunk("stderr", "final stderr\n") + end,
      ),
    );

    render(<LogsPane selector="loop_1" />);

    await vi.waitFor(() => {
      expect(screen.getByText("ended")).toBeTruthy();
    });
    expect(screen.getByText(/final stderr/)).toBeTruthy();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(apiMocks.openLoopLogsStream).toHaveBeenCalledTimes(1);
  });
});
