import { describe, expect, it } from "vitest";
import { formatAttempts, formatDurationSeconds, truncateReason } from "./format";

describe("formatDurationSeconds", () => {
  it("formats a daemon-provided duration without consulting the browser clock", () => {
    expect(formatDurationSeconds(0)).toBe("0s");
    expect(formatDurationSeconds(59)).toBe("59s");
    expect(formatDurationSeconds(26 * 60)).toBe("26m");
    expect(formatDurationSeconds(3 * 60 * 60)).toBe("3h");
    expect(formatDurationSeconds(49 * 60 * 60)).toBe("2d");
  });

  it("handles absent, invalid, and future values safely", () => {
    expect(formatDurationSeconds(undefined)).toBe("—");
    expect(formatDurationSeconds(Number.NaN)).toBe("—");
    expect(formatDurationSeconds(-1)).toBe("0s");
  });
});

describe("formatAttempts", () => {
  it("formats current/max including unlimited -1", () => {
    expect(formatAttempts(2, 5)).toBe("2/5");
    expect(formatAttempts(1, -1)).toBe("1/-1");
    expect(formatAttempts(0, 3)).toBe("0/3");
  });

  it("returns current only when max is missing", () => {
    expect(formatAttempts(2, null)).toBe("2");
    expect(formatAttempts(2, undefined)).toBe("2");
  });

  it("returns null when attempts metadata is absent", () => {
    expect(formatAttempts(null, 3)).toBeNull();
    expect(formatAttempts(undefined, -1)).toBeNull();
    expect(formatAttempts(Number.NaN, 3)).toBeNull();
  });
});

describe("truncateReason", () => {
  it("returns null for empty/missing", () => {
    expect(truncateReason(null)).toBeNull();
    expect(truncateReason(undefined)).toBeNull();
    expect(truncateReason("")).toBeNull();
    expect(truncateReason("   \n\t  ")).toBeNull();
  });

  it("collapses whitespace and truncates with ellipsis", () => {
    expect(truncateReason("agent idle\n timed  out", 64)).toBe(
      "agent idle timed out",
    );
    const long = "x".repeat(80);
    expect(truncateReason(long, 20)).toBe(`${"x".repeat(17)}...`);
  });

  it("does not truncate short text", () => {
    expect(truncateReason("dirty worktree", 64)).toBe("dirty worktree");
  });
});
