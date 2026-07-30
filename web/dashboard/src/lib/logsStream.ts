/** Backoff delays for unexpected SSE disconnects (ms). */
export const RECONNECT_BACKOFF_MS = [1000, 2000, 5000] as const;

export type LogsStreamPhase = "idle" | "connecting" | "live";

export type LogsStreamStatus =
  | "idle"
  | "connecting"
  | "live"
  | "ended"
  | "degraded";

export type LogsStreamErrorEvent = {
  code: string;
  message: string;
  retryable: boolean;
  retryAfterMs: number;
};

/**
 * UI status for the logs pane.
 * "connecting" wins over retained buffer text so reconnect never shows "live"
 * until a successful snapshot/chunk arrives on the new connection.
 */
export function resolveLogsStreamStatus(opts: {
  phase: LogsStreamPhase;
  ended: boolean;
  error: string | null;
}): LogsStreamStatus {
  if (opts.error) return "degraded";
  if (opts.ended) return "ended";
  if (opts.phase === "connecting") return "connecting";
  if (opts.phase === "live") return "live";
  return "idle";
}

/** Bounded reconnect delay for attempt index 0, 1, 2, ... */
export function nextReconnectDelayMs(
  attempt: number,
  delays: readonly number[] = RECONNECT_BACKOFF_MS,
): number {
  if (delays.length === 0) return 0;
  const idx = Math.max(0, Math.min(attempt, delays.length - 1));
  return delays[idx] ?? delays[delays.length - 1]!;
}

/** Apply a server retry floor while preserving the client's bounded cap. */
export function nextReconnectDelayAfterErrorMs(
  attempt: number,
  retryAfterMs: number,
  delays: readonly number[] = RECONNECT_BACKOFF_MS,
): number {
  const clientDelay = nextReconnectDelayMs(attempt, delays);
  const maxDelay = delays.length > 0 ? Math.max(...delays) : 0;
  const serverDelay = Number.isFinite(retryAfterMs)
    ? Math.max(0, Math.min(retryAfterMs, maxDelay))
    : 0;
  return Math.max(clientDelay, serverDelay);
}

/** Strictly decode the server's typed mid-stream SSE failure payload. */
export function parseLogsStreamError(rawData: string): LogsStreamErrorEvent {
  let raw: unknown;
  try {
    raw = JSON.parse(rawData);
  } catch {
    throw new Error("Malformed stream error event (invalid JSON)");
  }
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
    throw new Error("Malformed stream error event (expected object)");
  }

  const value = raw as Record<string, unknown>;
  if (typeof value.code !== "string" || !value.code.trim()) {
    throw new Error("Malformed stream error event (code is required)");
  }
  if (typeof value.message !== "string" || !value.message.trim()) {
    throw new Error("Malformed stream error event (message is required)");
  }
  if (typeof value.retryable !== "boolean") {
    throw new Error("Malformed stream error event (retryable must be boolean)");
  }

  const retryAfterMs = value.retryAfterMs ?? 0;
  if (
    typeof retryAfterMs !== "number" ||
    !Number.isFinite(retryAfterMs) ||
    retryAfterMs < 0
  ) {
    throw new Error(
      "Malformed stream error event (retryAfterMs must be non-negative)",
    );
  }
  return {
    code: value.code,
    message: value.message,
    retryable: value.retryable,
    retryAfterMs,
  };
}

/**
 * Prefix the first live stderr chunk with the same section header used by the
 * snapshot seed when stderr was empty at connect time.
 */
export function formatLiveStderrChunk(
  content: string,
  sectionHeaderPresent: boolean,
): string {
  if (!content) return "";
  if (sectionHeaderPresent) return content;
  return `\n--- stderr ---\n${content}`;
}
