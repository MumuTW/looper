import { describe, expect, it } from "vitest";

import { formatDurableProgress, formatPrimaryFailure } from "./fixerOutcome";

describe("formatPrimaryFailure", () => {
  it("returns null when there is nothing to show", () => {
    expect(formatPrimaryFailure(undefined)).toBeNull();
    expect(formatPrimaryFailure(null)).toBeNull();
    expect(formatPrimaryFailure({})).toBeNull();
    expect(formatPrimaryFailure({ primaryFailure: {} })).toBeNull();
    expect(
      formatPrimaryFailure({ primaryFailure: { step: "   ", message: "  " } }),
    ).toBeNull();
  });

  it("pairs the step with the message", () => {
    expect(
      formatPrimaryFailure({
        primaryFailure: { step: "repair", message: "agent timed out" },
      }),
    ).toBe("repair — agent timed out");
  });

  it("falls back to whichever half is present", () => {
    expect(formatPrimaryFailure({ primaryFailure: { step: "push" } })).toBe(
      "push",
    );
    expect(
      formatPrimaryFailure({ primaryFailure: { message: "boom" } }),
    ).toBe("boom");
  });

  it("counts later failures so the primary one is not mistaken for the only one", () => {
    expect(
      formatPrimaryFailure({
        primaryFailure: { step: "repair", message: "agent timed out" },
        secondaryIssues: [{ step: "push", message: "remote moved" }],
      }),
    ).toBe("repair — agent timed out (+1 later issue)");
    expect(
      formatPrimaryFailure({
        primaryFailure: { step: "repair", message: "agent timed out" },
        secondaryIssues: [{ step: "push" }, { step: "recheck" }],
      }),
    ).toBe("repair — agent timed out (+2 later issues)");
  });
});

describe("formatDurableProgress", () => {
  it("returns null when nothing durable happened", () => {
    expect(formatDurableProgress(undefined)).toBeNull();
    expect(formatDurableProgress({})).toBeNull();
    expect(formatDurableProgress({ progress: {} })).toBeNull();
    expect(
      formatDurableProgress({
        progress: { commitProduced: false, pushed: false, repliesSent: 0 },
      }),
    ).toBeNull();
  });

  it("lists only the effects that occurred", () => {
    expect(
      formatDurableProgress({
        progress: { commitProduced: true, pushed: true, repliesSent: 2, threadsResolved: 1 },
      }),
    ).toBe("committed · pushed · 2 replies · 1 thread");
    expect(formatDurableProgress({ progress: { pushed: true } })).toBe("pushed");
  });

  it("singularises counts", () => {
    expect(
      formatDurableProgress({ progress: { repliesSent: 1, threadsResolved: 2 } }),
    ).toBe("1 reply · 2 threads");
  });
});
