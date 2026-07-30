import { describe, expect, it } from "vitest";
import type { LoopEvent } from "@/lib/api";
import {
  isAwaitingHuman,
  isResumeStalled,
  latestEscalationCriteria,
  parseHITLAsk,
  questionWithoutRenderedCriteria,
  PLANNER_ESCALATION_EVENT_TYPE,
  type FiredCriterion,
} from "@/lib/hitl";

function metadata(hitl: unknown): string {
  return JSON.stringify({ projectId: "acme", hitl });
}

function event(partial: Partial<LoopEvent>): LoopEvent {
  return {
    id: "evt_1",
    eventType: PLANNER_ESCALATION_EVENT_TYPE,
    payloadJson: "{}",
    createdAt: "2026-07-30T10:00:00.000Z",
    ...partial,
  };
}

describe("parseHITLAsk", () => {
  it("reads the ask from loop metadata", () => {
    const ask = parseHITLAsk(
      metadata({
        question: "Authorize Planner to write a spec?",
        options: ["proceed: yes", "stop: no"],
        status: "awaiting",
        recommendation: "23 files across 4 packages.",
        recommendedOption: "proceed: yes",
        confidence: "medium",
        consequences: { "proceed: yes": "Planner writes the spec." },
        askedAt: "2026-07-30T10:00:00.000Z",
      }),
    );

    expect(ask?.question).toBe("Authorize Planner to write a spec?");
    expect(ask?.options).toEqual(["proceed: yes", "stop: no"]);
    expect(ask?.status).toBe("awaiting");
    expect(ask?.consequences).toEqual([
      { label: "proceed: yes", text: "Planner writes the spec." },
    ]);
    expect(isAwaitingHuman(ask)).toBe(true);
  });

  // metadataJson is a raw string from the daemon: a bad parse must read as
  // "no ask" so the loop page still renders.
  it("returns null for missing, malformed, or hitl-less metadata", () => {
    expect(parseHITLAsk(null)).toBeNull();
    expect(parseHITLAsk(undefined)).toBeNull();
    expect(parseHITLAsk("   ")).toBeNull();
    expect(parseHITLAsk("{not json")).toBeNull();
    expect(parseHITLAsk("[1,2,3]")).toBeNull();
    expect(parseHITLAsk('{"projectId":"acme"}')).toBeNull();
    expect(parseHITLAsk(metadata("awaiting"))).toBeNull();
  });

  it("reads the delivered answer once the ask has one", () => {
    const ask = parseHITLAsk(
      metadata({
        question: "Authorize Planner to write a spec?",
        status: "answered",
        answer: "proceed: yes",
        answeredAt: "2026-07-30T10:04:00.000Z",
      }),
    );

    expect(ask?.answer).toBe("proceed: yes");
    expect(ask?.answeredAt).toBe("2026-07-30T10:04:00.000Z");
  });

  it("coerces wrong-typed fields instead of trusting them", () => {
    const ask = parseHITLAsk(
      metadata({
        question: 42,
        options: ["ok", 7, "  ", null],
        status: "AWAITING",
        answer: { text: "proceed" },
        answeredAt: 1754000000,
        consequences: { ok: 5, "": "dropped", keep: "kept" },
      }),
    );

    expect(ask?.question).toBe("");
    expect(ask?.options).toEqual(["ok"]);
    expect(ask?.status).toBe("awaiting");
    expect(ask?.answer).toBe("");
    expect(ask?.answeredAt).toBe("");
    expect(ask?.consequences).toEqual([{ label: "keep", text: "kept" }]);
  });

  it("treats answered and consumed asks as no longer awaiting", () => {
    expect(isAwaitingHuman(parseHITLAsk(metadata({ status: "answered" })))).toBe(
      false,
    );
    expect(isAwaitingHuman(parseHITLAsk(metadata({ status: "consumed" })))).toBe(
      false,
    );
    expect(isAwaitingHuman(null)).toBe(false);
  });
});

describe("isResumeStalled", () => {
  const answered = parseHITLAsk(
    metadata({ status: "answered", answer: "proceed: yes" }),
  );

  // /respond writes the answer and flips the status in two steps: an answered
  // ask under a loop still parked as awaiting_human is the failure in between.
  it("is true only for an answered ask still parked as awaiting_human", () => {
    expect(isResumeStalled(answered, "awaiting_human")).toBe(true);
    expect(isResumeStalled(answered, "AWAITING_HUMAN")).toBe(true);
  });

  // The whole successful window — answered, then consumed by the resumed run —
  // must not read as stalled, or every answer would flash the failure card.
  it("is false once the loop left awaiting_human", () => {
    expect(isResumeStalled(answered, "running")).toBe(false);
    expect(isResumeStalled(answered, "completed")).toBe(false);
    expect(isResumeStalled(answered, null)).toBe(false);
    expect(isResumeStalled(answered, undefined)).toBe(false);
  });

  it("is false for asks that are not answered", () => {
    expect(
      isResumeStalled(parseHITLAsk(metadata({ status: "awaiting" })), "awaiting_human"),
    ).toBe(false);
    expect(
      isResumeStalled(parseHITLAsk(metadata({ status: "consumed" })), "awaiting_human"),
    ).toBe(false);
    expect(isResumeStalled(null, "awaiting_human")).toBe(false);
  });
});

describe("questionWithoutRenderedCriteria", () => {
  const criterion = (name: string): FiredCriterion => ({
    criterion: name,
    threshold: "",
    observed: "",
    evidence: [],
  });
  const question =
    "Authorize Planner to write a spec for acme/looper#42? Repository exploration tripped 2 escalation criteria before spec authoring:\n- blast_radius_files (maxFilesTouched=8): 23 files\n- adr_conflict (onAdrConflict=true): docs/adr/0007.md";

  it("drops the trailing criteria lines the card renders in full", () => {
    expect(
      questionWithoutRenderedCriteria(question, [
        criterion("blast_radius_files"),
        criterion("adr_conflict"),
      ]),
    ).toBe(
      // The trailing colon introduced the removed lines, so it goes with them.
      "Authorize Planner to write a spec for acme/looper#42? Repository exploration tripped 2 escalation criteria before spec authoring",
    );
  });

  // Nothing may leave the card: a line the criteria section is not showing
  // stays in the question, and so does everything above it.
  it("keeps lines that name a criterion not being rendered", () => {
    expect(
      questionWithoutRenderedCriteria(question, [
        criterion("blast_radius_files"),
      ]),
    ).toBe(question);
    expect(questionWithoutRenderedCriteria(question, [])).toBe(question);
  });

  it("leaves questions that are not a criteria enumeration alone", () => {
    const plain = "Apply the migration to production now?";
    expect(
      questionWithoutRenderedCriteria(plain, [criterion("adr_conflict")]),
    ).toBe(plain);
    expect(
      questionWithoutRenderedCriteria(
        "- adr_conflict is the whole question",
        [criterion("adr_conflict")],
      ),
    ).toBe("- adr_conflict is the whole question");
    expect(questionWithoutRenderedCriteria("", [criterion("x")])).toBe("");
  });
});

describe("latestEscalationCriteria", () => {
  it("reads criteria from the newest planner.escalation event", () => {
    const criteria = latestEscalationCriteria([
      event({
        id: "evt_old",
        createdAt: "2026-07-30T09:00:00.000Z",
        payload: { criteria: [{ criterion: "adr_conflict" }] },
      }),
      event({ id: "evt_other", eventType: "run.started", payload: {} }),
      event({
        id: "evt_new",
        createdAt: "2026-07-30T11:00:00.000Z",
        payload: {
          criteria: [
            {
              criterion: "blast_radius_files",
              threshold: "maxFilesTouched=8",
              observed: "23 files",
              evidence: ["internal/api/handler.go", 7, "  "],
            },
          ],
        },
      }),
    ]);

    expect(criteria).toEqual([
      {
        criterion: "blast_radius_files",
        threshold: "maxFilesTouched=8",
        observed: "23 files",
        evidence: ["internal/api/handler.go"],
      },
    ]);
  });

  it("falls back to payloadJson when the daemon could not parse the payload", () => {
    const criteria = latestEscalationCriteria([
      event({
        payload: "{not json",
        payloadJson: JSON.stringify({
          criteria: [{ criterion: "adr_conflict", observed: "docs/adr/003.md" }],
        }),
      }),
    ]);

    expect(criteria).toEqual([
      {
        criterion: "adr_conflict",
        threshold: "",
        observed: "docs/adr/003.md",
        evidence: [],
      },
    ]);
  });

  it("returns [] for loops with no escalation and for unusable payloads", () => {
    expect(latestEscalationCriteria([])).toEqual([]);
    expect(latestEscalationCriteria(null)).toEqual([]);
    expect(
      latestEscalationCriteria([event({ eventType: "run.completed" })]),
    ).toEqual([]);
    expect(latestEscalationCriteria([event({ payload: { criteria: {} } })])).toEqual(
      [],
    );
    expect(
      latestEscalationCriteria([event({ payload: { criteria: [{}, null] } })]),
    ).toEqual([]);
  });
});
