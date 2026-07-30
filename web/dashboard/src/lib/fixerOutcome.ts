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
