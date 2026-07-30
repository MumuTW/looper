import { describe, expect, it } from "vitest";

import { formatPrimaryFailure } from "./fixerOutcome";

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
