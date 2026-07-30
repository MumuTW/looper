import type { FixerRunOutcome } from "@/lib/api";

export function fixerOutcomeLabel(outcome: FixerRunOutcome): string {
  if (outcome.partialSuccess) return "Partial success";
  if (outcome.primaryFailure) return "Failed without durable progress";
  if ((outcome.secondaryIssues?.length ?? 0) > 0) {
    return "Completed with cleanup issues";
  }
  if ((outcome.followUpThreadIds?.length ?? 0) > 0) {
    return "Completed with follow-up work";
  }
  return "Completed";
}

export function fixerOutcomeProgress(outcome: FixerRunOutcome): string {
  const parts: string[] = [];
  if (outcome.progress.commitProduced) parts.push("commit produced");
  if (outcome.progress.pushed) parts.push("pushed");
  if ((outcome.progress.repliesSent ?? 0) > 0) {
    parts.push(`${outcome.progress.repliesSent} replies`);
  }
  if ((outcome.progress.threadsResolved ?? 0) > 0) {
    parts.push(`${outcome.progress.threadsResolved} threads resolved`);
  }
  return parts.length > 0 ? parts.join(" · ") : "No durable progress";
}
