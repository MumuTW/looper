import type { FixerRunOutcome } from "./api";

/**
 * Renders the causal failure as "step — message", with a count when later failures
 * piled on. Returns null when there is nothing to show, so callers can fall back to
 * the loop's own last-failure fields rather than printing an empty row.
 *
 * The step is included because it is the part the loop's `lastFailureReason` cannot
 * carry: a run that failed at resolve-comments after pushing reads very differently
 * from one that failed at repair, and the message alone rarely says which.
 */
export function formatPrimaryFailure(
  outcome: FixerRunOutcome | null | undefined,
): string | null {
  const primary = outcome?.primaryFailure;
  if (!primary) return null;

  const step = primary.step?.trim();
  const message = primary.message?.trim();
  if (!step && !message) return null;

  const head = step && message ? `${step} — ${message}` : (step ?? message ?? "");
  const later = outcome?.secondaryIssues?.length ?? 0;
  if (later < 1) return head;
  return `${head} (+${later} later ${later === 1 ? "issue" : "issues"})`;
}

/**
 * Summarises what a run actually left behind, e.g. "pushed · 2 replies · 1 thread".
 * Returns null when nothing durable happened, so callers show a dash rather than an
 * empty or falsely reassuring string.
 *
 * Only non-zero effects appear: a list of zeroes reads as detail while saying
 * nothing, and the absence of an effect is already carried by its absence.
 */
export function formatDurableProgress(
  outcome: FixerRunOutcome | null | undefined,
): string | null {
  const progress = outcome?.progress;
  if (!progress) return null;

  const parts: string[] = [];
  if (progress.commitProduced) parts.push("committed");
  if (progress.pushed) parts.push("pushed");

  const replies = progress.repliesSent ?? 0;
  if (replies > 0) parts.push(`${replies} ${replies === 1 ? "reply" : "replies"}`);

  const threads = progress.threadsResolved ?? 0;
  if (threads > 0) parts.push(`${threads} ${threads === 1 ? "thread" : "threads"}`);

  return parts.length > 0 ? parts.join(" · ") : null;
}
