import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Link, useParams } from "react-router-dom";
import { LoopActionBar } from "@/components/LoopActionBar";
import { PanelError } from "@/components/PanelError";
import { StatusChip } from "@/components/StatusChip";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  formatDurableProgress,
  formatPrimaryFailure,
  formatSecondaryIssues,
} from "@/lib/fixerOutcome";
import {
  fetchLoop,
  openLoopLogsStream,
  type ActiveRunProgress,
  type Loop,
  type LoopLogsChunk,
  type LoopLogsSnapshot,
  type ReviewerConvergence,
} from "@/lib/api";
import { useDashboardData } from "@/lib/DashboardDataContext";
import { formatAttempts, formatTs } from "@/lib/format";
import { capLogChunk, capLogSeed, trimLogBuffer } from "@/lib/logBuffer";
import {
  type LogsStreamErrorEvent,
  type LogsStreamPhase,
  formatLiveStderrChunk,
  nextReconnectDelayAfterErrorMs,
  nextReconnectDelayMs,
  parseLogsStreamError,
  resolveLogsStreamStatus,
} from "@/lib/logsStream";
import { consumeSSE } from "@/lib/sse";
import { usePolling } from "@/lib/usePolling";

function seedFromSnapshot(snap: LoopLogsSnapshot): string {
  const agent = snap.agent;
  if (!agent) {
    return "(no agent output yet)\n";
  }
  // Cap string log fields after parse (not the raw SSE JSON envelope).
  const stdout = agent.stdout ? capLogSeed(agent.stdout) : "";
  const stderr = agent.stderr ? capLogSeed(agent.stderr) : "";
  const parts: string[] = [];
  if (stdout) parts.push(stdout);
  if (stderr) {
    if (parts.length && !parts[parts.length - 1].endsWith("\n")) {
      parts.push("\n");
    }
    parts.push("--- stderr ---\n");
    parts.push(stderr);
  }
  if (parts.length === 0) {
    return "(empty snapshot)\n";
  }
  return parts.join("");
}

function Kv({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="grid grid-cols-[110px_1fr] gap-2 py-0.5 text-[12px]">
      <dt className="text-[var(--text-muted)]">{label}</dt>
      <dd className="m-0 break-all mono">{value}</dd>
    </div>
  );
}

// severityFloorIncludes mirrors convergence.Policy.Includes on the runtime
// side: an item counts toward convergence progress only when its severity is
// at or above the configured floor. Keeping this in sync prevents the card
// from listing below-floor items as open while the projection reports
// completion.
function severityFloorIncludes(
  floor: string | null | undefined,
  severity: string | null | undefined,
): boolean {
  switch (floor) {
    case "blocking":
      return severity === "blocking";
    case "non_blocking":
      return severity === "blocking" || severity === "non_blocking";
    case "all":
      return (
        severity === "blocking" ||
        severity === "non_blocking" ||
        severity === "nit"
      );
    default:
      return false;
  }
}

function ReviewerConvergenceCard({
  convergence,
}: {
  convergence: ReviewerConvergence;
}) {
  const items = Object.values(convergence.state.items ?? {}).sort((a, b) =>
    a.id.localeCompare(b.id),
  );
  // Mirror convergence.Policy.Includes: an item below the severity floor is
  // non-blocking to progress even when still open, so listing it alongside
  // floor-scoped open work would let the card report completion and apparent
  // open convergence work at the same time.
  const openItems = items.filter(
    (item) =>
      item.status === "open" &&
      !item.stuck &&
      severityFloorIncludes(convergence.policy.severityFloor, item.severity),
  );
  const recentHistory = (convergence.state.history ?? []).slice(-8);

  return (
    <Card title="Reviewer convergence">
      <dl className="m-0 columns-1 gap-x-6 md:columns-2">
        <Kv label="Status" value={convergence.status ?? "active"} />
        <Kv label="Action" value={convergence.action ?? "—"} />
        <Kv label="Reason" value={convergence.reason ?? "—"} />
        <Kv
          label="Rounds"
          value={`${convergence.state.totalRounds} / ${convergence.policy.maxTotalRounds}`}
        />
        <Kv
          label="Unproductive"
          value={`${convergence.state.consecutiveUnproductive} / ${convergence.policy.maxConsecutiveUnproductive}`}
        />
        <Kv label="Severity floor" value={convergence.policy.severityFloor} />
        <Kv label="Updated" value={formatTs(convergence.updatedAt)} />
      </dl>

      <div className="mt-2 border-t border-[var(--border)] pt-2 text-[12px]">
        <p className="m-0 mb-1 font-medium">Open items</p>
        {openItems.length > 0 ? (
          <ul className="m-0 list-none space-y-0.5 p-0">
            {openItems.map((item) => (
              <li key={item.id} className="flex flex-wrap gap-x-2 gap-y-0.5">
                <span className="mono">{item.id}</span>
                <span className="text-[var(--text-muted)]">
                  {item.severity} · {item.status} ·{" "}
                  {(item.fixerAttempts ?? 0)} /{" "}
                  {convergence.policy.maxFixerAttemptsPerItem} fixer attempts
                </span>
              </li>
            ))}
          </ul>
        ) : (
          <p className="m-0 text-[var(--text-muted)]">None</p>
        )}
      </div>

      <div className="mt-2 border-t border-[var(--border)] pt-2 text-[12px]">
        <p className="m-0 mb-1 font-medium">Productivity trend</p>
        {recentHistory.length > 0 ? (
          <div className="flex flex-wrap gap-1.5">
            {recentHistory.map((round) => (
              <span
                key={round.number}
                className="rounded border border-[var(--border)] px-1.5 py-0.5 mono text-[11px]"
                title={`open items: ${(round.openItemIds ?? []).join(", ") || "none"}`}
              >
                #{round.number} {round.productive ? "productive" : "unproductive"}
              </span>
            ))}
          </div>
        ) : (
          <p className="m-0 text-[var(--text-muted)]">No rounds recorded.</p>
        )}
      </div>
    </Card>
  );
}

function formatTimeoutProgress(progress?: ActiveRunProgress | null): string {
  if (!progress) return "—";
  const head = progress.headSha?.trim() || "unknown HEAD";
  const branch = progress.branch?.trim() || "unknown branch";
  const files = `${progress.changedFileCount}/${progress.stagedFileCount}/${progress.untrackedFileCount} changed/staged/untracked`;
  const fingerprint = progress.diffFingerprint?.trim();
  return `${head} · ${branch} · ${files}${fingerprint ? ` · ${fingerprint}` : ""}`;
}

export function LogsPane({ selector }: { selector: string }) {
  const [text, setText] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [phase, setPhase] = useState<LogsStreamPhase>("idle");
  const [autoScroll, setAutoScroll] = useState(true);
  const [ended, setEnded] = useState(false);
  const preRef = useRef<HTMLPreElement | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const autoScrollRef = useRef(autoScroll);
  autoScrollRef.current = autoScroll;
  const explicitEndRef = useRef(false);
  const streamErrorRef = useRef<LogsStreamErrorEvent | null>(null);
  const reconnectAttemptRef = useRef(0);
  const reconnectTimerRef = useRef<number | null>(null);
  const generationRef = useRef(0);

  const scrollToBottom = useCallback(() => {
    if (!autoScrollRef.current) return;
    const el = preRef.current;
    if (!el) return;
    requestAnimationFrame(() => {
      el.scrollTop = el.scrollHeight;
    });
  }, []);

  const replaceText = useCallback(
    (next: string) => {
      setText(trimLogBuffer(next));
      scrollToBottom();
    },
    [scrollToBottom],
  );

  const appendText = useCallback(
    (chunk: string) => {
      const capped = capLogChunk(chunk);
      setText((prev) => trimLogBuffer(prev + capped));
      scrollToBottom();
    },
    [scrollToBottom],
  );

  const clearReconnectTimer = useCallback(() => {
    if (reconnectTimerRef.current != null) {
      window.clearTimeout(reconnectTimerRef.current);
      reconnectTimerRef.current = null;
    }
  }, []);

  const stopStream = useCallback(() => {
    clearReconnectTimer();
    abortRef.current?.abort();
    abortRef.current = null;
    setPhase("idle");
  }, [clearReconnectTimer]);

  const startStream = useCallback(
    (opts?: { isReconnect?: boolean }) => {
      clearReconnectTimer();
      abortRef.current?.abort();
      abortRef.current = null;

      if (!opts?.isReconnect) {
        reconnectAttemptRef.current = 0;
      }

      explicitEndRef.current = false;
      streamErrorRef.current = null;
      setError(null);
      setEnded(false);
      // Connecting until first successful snapshot/chunk on this connection.
      // Retained prior log text must not imply "live".
      setPhase("connecting");

      const generation = ++generationRef.current;
      const controller = new AbortController();
      abortRef.current = controller;
      let reconnectSettled = false;
      let streamEnded = false;
      let stderrSectionHeaderPresent = false;

      const recordStreamError = (streamError: LogsStreamErrorEvent) => {
        streamErrorRef.current = streamError;
        setError(streamError.message);
        setPhase("idle");
      };

      const decodeStreamError = (rawData: string): LogsStreamErrorEvent => {
        try {
          return parseLogsStreamError(rawData);
        } catch (err) {
          return {
            code: "MALFORMED_STREAM_ERROR",
            message:
              err instanceof Error
                ? err.message
                : "Malformed stream error event",
            retryable: true,
            retryAfterMs: 0,
          };
        }
      };

      const disconnectedStreamError = (err: unknown): LogsStreamErrorEvent => ({
        code: "STREAM_DISCONNECTED",
        message:
          err instanceof Error
            ? err.message
            : "logs stream disconnected",
        retryable: true,
        retryAfterMs: 0,
      });

      const scheduleReconnect = () => {
        if (explicitEndRef.current) return;
        if (
          typeof document !== "undefined" &&
          document.visibilityState === "hidden"
        ) {
          return;
        }
        if (generation !== generationRef.current) return;
        if (reconnectSettled) return;
        reconnectSettled = true;

        const streamError = streamErrorRef.current;
        streamErrorRef.current = null;
        if (streamError && !streamError.retryable) return;

        const attempt = reconnectAttemptRef.current;
        const delay = streamError
          ? nextReconnectDelayAfterErrorMs(
              attempt,
              streamError.retryAfterMs,
            )
          : nextReconnectDelayMs(attempt);
        reconnectAttemptRef.current = attempt + 1;
        clearReconnectTimer();
        setPhase("connecting");
        reconnectTimerRef.current = window.setTimeout(() => {
          if (generation !== generationRef.current) return;
          if (document.visibilityState === "hidden") return;
          startStream({ isReconnect: true });
        }, delay);
      };

      void (async () => {
        try {
          const response = await openLoopLogsStream(selector, controller.signal);
          await consumeSSE(
            response,
            (event, rawData) => {
              if (generation !== generationRef.current) return;
              if (event === "snapshot") {
                try {
                  const snap = JSON.parse(rawData) as LoopLogsSnapshot;
                  replaceText(seedFromSnapshot(snap));
                  stderrSectionHeaderPresent = Boolean(
                    snap.agent?.stderr?.trim(),
                  );
                  setPhase("live");
                } catch {
                  setError("Malformed snapshot event (invalid JSON)");
                  setPhase("idle");
                }
                return;
              }
              if (event === "chunk") {
                try {
                  const chunk = JSON.parse(rawData) as LoopLogsChunk;
                  if (typeof chunk.content === "string" && chunk.content) {
                    if (chunk.stream === "stderr") {
                      appendText(
                        formatLiveStderrChunk(
                          chunk.content,
                          stderrSectionHeaderPresent,
                        ),
                      );
                      stderrSectionHeaderPresent = true;
                    } else {
                      appendText(chunk.content);
                    }
                  }
                  setPhase("live");
                } catch {
                  setError("Malformed chunk event (invalid JSON)");
                  setPhase("idle");
                }
                return;
              }
              if (event === "end") {
                streamEnded = true;
                explicitEndRef.current = true;
                setEnded(true);
                setPhase("idle");
                return;
              }
              if (event === "error") {
                recordStreamError(decodeStreamError(rawData));
              }
            },
            controller.signal,
          );
          if (
            controller.signal.aborted ||
            generation !== generationRef.current
          ) {
            return;
          }
          if (streamEnded) return;
          setPhase("idle");
          // Unexpected stream end (no explicit end event) → reconnect while visible.
          if (!explicitEndRef.current) {
            scheduleReconnect();
          }
        } catch (err) {
          if (
            controller.signal.aborted ||
            generation !== generationRef.current
          ) {
            return;
          }
          if (err instanceof Error && err.name === "AbortError") return;
          if (err instanceof DOMException && err.name === "AbortError") return;
          recordStreamError(disconnectedStreamError(err));
          if (!explicitEndRef.current) {
            scheduleReconnect();
          }
        }
      })();
    },
    [appendText, clearReconnectTimer, replaceText, selector],
  );

  // Start / stop based on visibility; cancel reconnects when hidden/unmount.
  useEffect(() => {
    const onVisibility = () => {
      if (document.visibilityState === "hidden") {
        stopStream();
      } else {
        startStream();
      }
    };

    if (document.visibilityState === "visible") {
      startStream();
    }

    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      document.removeEventListener("visibilitychange", onVisibility);
      stopStream();
    };
  }, [startStream, stopStream]);

  const onClear = () => setText("");

  const onCopy = async () => {
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      const ta = document.createElement("textarea");
      ta.value = text;
      document.body.appendChild(ta);
      ta.select();
      document.execCommand("copy");
      document.body.removeChild(ta);
    }
  };

  const status = resolveLogsStreamStatus({ phase, ended, error });

  return (
    <Card
      title="Logs"
      actions={
        <div className="flex flex-wrap items-center gap-1">
          <span className="mr-1 text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
            {status}
          </span>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setAutoScroll((v) => !v)}
          >
            {autoScroll ? "Pause scroll" : "Resume scroll"}
          </Button>
          <Button variant="ghost" size="sm" onClick={onClear}>
            Clear
          </Button>
          <Button variant="ghost" size="sm" onClick={() => void onCopy()}>
            Copy
          </Button>
          <Button variant="ghost" size="sm" onClick={() => startStream()}>
            Reconnect
          </Button>
        </div>
      }
    >
      {error ? (
        <div className="mb-2">
          <PanelError message={error} onRetry={() => startStream()} />
        </div>
      ) : null}
      <pre
        ref={preRef}
        className="mono m-0 max-h-[min(60vh,520px)] overflow-auto whitespace-pre-wrap break-words rounded border border-[var(--border)] bg-[var(--bg)] p-2 text-[11px] leading-snug text-[var(--text)]"
      >
        {text || (phase === "connecting" ? "Connecting…" : "—")}
      </pre>
    </Card>
  );
}

export function LoopDetailPage() {
  const { selector = "" } = useParams<{ selector: string }>();
  const { activeRuns } = useDashboardData();

  const fetcher = useCallback(
    (signal: AbortSignal) => fetchLoop(selector, signal),
    [selector],
  );
  const { data, error, loading, refresh, forceRefresh } = usePolling<Loop>({
    intervalMs: 3000,
    enabled: Boolean(selector),
    fetcher,
    key: selector,
  });

  const activeRunItems = activeRuns.data?.items;
  const forceRefreshActiveRuns = activeRuns.forceRefresh;

  const primaryFailure = useMemo(
    () => formatPrimaryFailure(data?.outcome),
    [data?.outcome],
  );
  const durableProgress = useMemo(
    () => formatDurableProgress(data?.outcome),
    [data?.outcome],
  );
  const secondaryIssues = useMemo(
    () => formatSecondaryIssues(data?.outcome),
    [data?.outcome],
  );

  const activeRun = useMemo(() => {
    if (!data) return null;
    const items = activeRunItems ?? [];
    return items.find(
      (r) => r.loopId === data.id || r.seq === data.seq,
    );
  }, [activeRunItems, data]);
  // The loop detail response carries the latest run's redacted continuation
  // evidence even after the run closes; fall back to the shared active-run
  // projection while a retry is still live.
  const continuation = data?.continuation ?? activeRun?.continuation;
  const hasActiveRun = Boolean(activeRun);

  const onMutated = useCallback(async () => {
    await Promise.all([forceRefresh(), forceRefreshActiveRuns()]);
  }, [forceRefresh, forceRefreshActiveRuns]);

  if (!selector) {
    return <PanelError message="Missing loop selector" />;
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <Link
            to="/loops"
            className="text-[12px] text-[var(--text-muted)] hover:text-[var(--text)]"
          >
            ← Loops
          </Link>
          <h1 className="m-0 text-[15px] font-semibold">
            Loop{" "}
            <span className="mono">{data ? `#${data.seq}` : selector}</span>
          </h1>
          {data ? <StatusChip status={data.status} /> : null}
        </div>
        <Button variant="ghost" size="sm" onClick={refresh}>
          Refresh
        </Button>
      </div>

      {data ? (
        <Card title="Actions">
          <LoopActionBar
            selector={String(data.seq)}
            status={data.status}
            hasActiveRun={hasActiveRun}
            onMutated={onMutated}
            mode="full"
          />
        </Card>
      ) : null}

      {continuation ? (
        <Card title="Timeout continuation">
          <dl className="m-0 columns-1 gap-x-6 md:columns-2">
            <Kv label="Mode" value={continuation.mode ?? "—"} />
            <Kv
              label="Outcome"
              value={
                continuation.outcome ??
                (continuation.mode === "timeout_observed"
                  ? "awaiting retry"
                  : "observation failed")
              }
            />
            <Kv
              label="Predecessor run"
              value={continuation.predecessorRunId ?? "—"}
            />
            <Kv
              label="Predecessor exec"
              value={continuation.predecessorExecutionId ?? "—"}
            />
            <Kv
              label="Before timeout"
              value={formatTimeoutProgress(continuation.beforeTimeout)}
            />
            <Kv
              label="Before retry"
              value={formatTimeoutProgress(continuation.afterRestart)}
            />
            <Kv
              label="Last progress"
              value={formatTs(continuation.beforeTimeout?.lastProgressAt)}
            />
          </dl>
        </Card>
      ) : null}

      <Card title="Metadata">
        {error && !data ? (
          <PanelError message={error} onRetry={refresh} />
        ) : loading && !data ? (
          <p className="m-0 text-[12px] text-[var(--text-muted)]">
            Loading loop…
          </p>
        ) : data ? (
          <>
            {error ? (
              <div className="mb-2">
                <PanelError message={error} onRetry={refresh} />
              </div>
            ) : null}
            <dl className="m-0 columns-1 gap-x-6 md:columns-2">
              <Kv label="Seq" value={data.seq} />
              <Kv label="ID" value={data.id} />
              <Kv label="Type" value={data.type} />
              <Kv label="Status" value={<StatusChip status={data.status} />} />
              <Kv label="Project" value={data.projectId} />
              <Kv label="Target type" value={data.targetType} />
              <Kv label="Target ID" value={data.targetId ?? "—"} />
              <Kv label="Repo" value={data.repo ?? "—"} />
              <Kv
                label="PR"
                value={data.prNumber != null ? String(data.prNumber) : "—"}
              />
              <Kv
                label="Attempts"
                value={
                  formatAttempts(data.attempts, data.maxAttempts) ?? "—"
                }
              />
              <Kv
                label="Error kind"
                value={
                  data.lastFailureKind?.trim()
                    ? data.lastFailureKind.trim()
                    : "—"
                }
              />
              <Kv
                label="Error / reason"
                value={
                  data.lastFailureReason?.trim() ? (
                    <span className="whitespace-pre-wrap break-words">
                      {data.lastFailureReason.trim()}
                    </span>
                  ) : (
                    "—"
                  )
                }
              />
              {/*
                The row above is the latest error from the queue. This one is the
                first, causal failure from the run, which differs exactly when a
                later problem piled on top of the real cause.
              */}
              <Kv
                label="First failure"
                value={
                  primaryFailure ? (
                    <span className="whitespace-pre-wrap break-words">
                      {primaryFailure}
                    </span>
                  ) : (
                    "—"
                  )
                }
              />
              {/*
                What survived the run. Paired with "First failure" this is the
                difference between "nothing shipped, retry freely" and "some of
                this already landed".
              */}
              <Kv
                label={
                  data.outcome?.partialSuccess ? "Kept (partial)" : "Kept"
                }
                value={durableProgress ?? "—"}
              />
              {/*
                Problems around the run's own result -- a refused cleanup, a failure
                while parking. Kept separate from "First failure" so the causal one
                stays easy to find.
              */}
              <Kv
                label="Also"
                value={
                  secondaryIssues ? (
                    <ul className="m-0 list-none space-y-0.5 p-0">
                      {secondaryIssues.map((issue) => (
                        <li
                          key={issue}
                          className="whitespace-pre-wrap break-words"
                        >
                          {issue}
                        </li>
                      ))}
                    </ul>
                  ) : (
                    "—"
                  )
                }
              />
              <Kv label="Last run" value={formatTs(data.lastRunAt)} />
              <Kv label="Next run" value={formatTs(data.nextRunAt)} />
              <Kv label="Created" value={formatTs(data.createdAt)} />
              <Kv label="Updated" value={formatTs(data.updatedAt)} />
            </dl>
          </>
        ) : (
          <p className="m-0 text-[12px] text-[var(--text-muted)]">No data</p>
        )}
      </Card>

      {data?.convergence ? (
        <ReviewerConvergenceCard convergence={data.convergence} />
      ) : null}

      {/* Remount on selector change so log buffer/stream state never leaks. */}
      <LogsPane key={selector} selector={selector} />
    </div>
  );
}
