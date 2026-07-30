import { describe, expect, it } from "vitest";
import {
  RECONNECT_BACKOFF_MS,
  formatLiveStderrChunk,
  nextReconnectDelayAfterErrorMs,
  nextReconnectDelayMs,
  parseLogsStreamError,
  resolveLogsStreamStatus,
} from "./logsStream";

describe("resolveLogsStreamStatus", () => {
  it("shows connecting while reconnecting even if prior logs are retained", () => {
    expect(
      resolveLogsStreamStatus({
        phase: "connecting",
        ended: false,
        error: null,
      }),
    ).toBe("connecting");
  });

  it("shows live only after phase advances past connecting", () => {
    expect(
      resolveLogsStreamStatus({
        phase: "live",
        ended: false,
        error: null,
      }),
    ).toBe("live");
  });

  it("prefers degraded and ended over phase", () => {
    expect(
      resolveLogsStreamStatus({
        phase: "connecting",
        ended: false,
        error: "boom",
      }),
    ).toBe("degraded");
    expect(
      resolveLogsStreamStatus({
        phase: "live",
        ended: true,
        error: null,
      }),
    ).toBe("ended");
  });
});

describe("nextReconnectDelayMs", () => {
  it("uses bounded backoff 1s, 2s, 5s max", () => {
    expect(nextReconnectDelayMs(0)).toBe(1000);
    expect(nextReconnectDelayMs(1)).toBe(2000);
    expect(nextReconnectDelayMs(2)).toBe(5000);
    expect(nextReconnectDelayMs(10)).toBe(5000);
    expect(RECONNECT_BACKOFF_MS).toEqual([1000, 2000, 5000]);
  });

  it("honors a typed server retry floor without exceeding the client cap", () => {
    expect(nextReconnectDelayAfterErrorMs(0, 3000)).toBe(3000);
    expect(nextReconnectDelayAfterErrorMs(1, 10_000)).toBe(5000);
    expect(nextReconnectDelayAfterErrorMs(10, 1000)).toBe(5000);
    expect(nextReconnectDelayAfterErrorMs(0, -1)).toBe(1000);
  });
});

describe("parseLogsStreamError", () => {
  it("decodes the typed mid-stream failure contract", () => {
    expect(
      parseLogsStreamError(
        JSON.stringify({
          code: "INTERNAL_ERROR",
          message: "database is locked",
          retryable: true,
          retryAfterMs: 1000,
        }),
      ),
    ).toEqual({
      code: "INTERNAL_ERROR",
      message: "database is locked",
      retryable: true,
      retryAfterMs: 1000,
    });
  });

  it("rejects malformed or incomplete error events", () => {
    expect(() => parseLogsStreamError("not-json")).toThrow();
    expect(() =>
      parseLogsStreamError(
        JSON.stringify({ code: "INTERNAL_ERROR", message: "boom" }),
      ),
    ).toThrow(/retryable/);
  });
});

describe("formatLiveStderrChunk", () => {
  it("adds a stderr section header only for the first live chunk", () => {
    expect(formatLiveStderrChunk("boom\n", false)).toBe(
      "\n--- stderr ---\nboom\n",
    );
    expect(formatLiveStderrChunk("more\n", true)).toBe("more\n");
    expect(formatLiveStderrChunk("", false)).toBe("");
  });
});
