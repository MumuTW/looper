import { describe, expect, it } from "vitest";

import { fixerOutcomeLabel, fixerOutcomeProgress } from "@/lib/fixerOutcome";

describe("fixer outcome presentation", () => {
  it("distinguishes historical partial success", () => {
    const outcome = {
      primaryFailure: { step: "recheck", message: "recheck failed", retryable: true },
      partialSuccess: true,
      progress: { commitProduced: true, pushed: true, repliesSent: 1, threadsResolved: 2 },
    };
    expect(fixerOutcomeLabel(outcome)).toBe("Partial success");
    expect(fixerOutcomeProgress(outcome)).toBe(
      "commit produced · pushed · 1 replies · 2 threads resolved",
    );
  });

  it("distinguishes blocked failures from follow-up completion", () => {
    expect(
      fixerOutcomeLabel({
        primaryFailure: { message: "GitHub auth failed", retryable: false },
        progress: {},
      }),
    ).toBe("Failed without durable progress");
    expect(
      fixerOutcomeLabel({ progress: {}, followUpThreadIds: ["thread-2"] }),
    ).toBe("Completed with follow-up work");
    expect(
      fixerOutcomeLabel({
        progress: { pushed: true },
        secondaryIssues: [{ message: "cleanup failed", retryable: true }],
      }),
    ).toBe("Completed with cleanup issues");
  });
});
