import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { HITLDecisionCard } from "./HITLDecisionCard";
import type { HITLAsk } from "@/lib/api";

vi.mock("@/lib/api", () => ({
  respondLoop: vi.fn(),
}));

vi.mock("@/lib/toast", () => ({
  useToast: () => ({
    success: vi.fn(),
    error: vi.fn(),
  }),
}));

afterEach(() => {
  cleanup();
});

function baseAsk(overrides: Partial<HITLAsk> = {}): HITLAsk {
  return {
    question: "Keep RollingUpdate or make strategy configurable?",
    options: ["keep RollingUpdate", "make configurable"],
    status: "awaiting",
    ...overrides,
  };
}

describe("HITLDecisionCard", () => {
  it("is interactive while loop is awaiting_human and ask is awaiting", () => {
    render(
      <HITLDecisionCard
        selector="42"
        loopStatus="awaiting_human"
        ask={baseAsk({ status: "awaiting" })}
      />,
    );
    expect(screen.getByRole("button", { name: /keep RollingUpdate/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Send answer/i })).toBeTruthy();
  });

  it("stays interactive when ask is answered but loop is still awaiting_human", () => {
    // /respond may persist answer then fail requeue (e.g. agent removed). Loop
    // remains parked; operator must retry through the dashboard after fix.
    render(
      <HITLDecisionCard
        selector="42"
        loopStatus="awaiting_human"
        ask={baseAsk({
          status: "answered",
          answer: "keep RollingUpdate",
        })}
      />,
    );
    expect(screen.getByRole("button", { name: /keep RollingUpdate/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Send answer/i })).toBeTruthy();
    expect(screen.queryByText(/^Answer$/i)).toBeNull();
  });

  it("is read-only once the loop leaves awaiting_human", () => {
    render(
      <HITLDecisionCard
        selector="42"
        loopStatus="running"
        ask={baseAsk({
          status: "answered",
          answer: "keep RollingUpdate",
        })}
      />,
    );
    expect(screen.queryByRole("button", { name: /Send answer/i })).toBeNull();
    expect(screen.getByText("keep RollingUpdate")).toBeTruthy();
  });

  it("is read-only when ask is consumed even if status string is stale", () => {
    render(
      <HITLDecisionCard
        selector="42"
        loopStatus="awaiting_human"
        ask={baseAsk({ status: "consumed", answer: "done" })}
      />,
    );
    expect(screen.queryByRole("button", { name: /Send answer/i })).toBeNull();
  });
});
