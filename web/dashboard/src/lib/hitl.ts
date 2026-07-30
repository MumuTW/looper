/**
 * Parsing for the mid-run human-in-the-loop ask that parks a loop as
 * awaiting_human, plus the Planner escalation record that explains why it
 * stopped.
 *
 * Both arrive as freeform JSON — the ask is a raw string on the loop
 * (metadataJson) and event payloads are typed `unknown` — so every field is
 * coerced rather than asserted. A malformed ask must read as "no ask" and
 * never break the loop page.
 */
import type { LoopEvent } from "@/lib/api";

/** Mirrors internal/loops.HITLAsk (the `hitl` key of a loop's metadataJson). */
export type HITLAsk = {
  question: string;
  options: string[];
  /** "awaiting" | "answered" | "consumed" (lowercased; "" when absent). */
  status: string;
  /** The delivered answer, recorded from "answered" on. */
  answer: string;
  recommendation: string;
  recommendedOption: string;
  confidence: string;
  consequences: ConsequenceEntry[];
  askedAt: string;
  answeredAt: string;
};

/** One entry of the ask's label→text consequences map, in JSON order. */
export type ConsequenceEntry = {
  label: string;
  text: string;
};

/** Mirrors internal/planner.FiredCriterion. */
export type FiredCriterion = {
  criterion: string;
  threshold: string;
  observed: string;
  evidence: string[];
};

/** internal/planner.EscalationEventType. */
export const PLANNER_ESCALATION_EVENT_TYPE = "planner.escalation";

const HITL_METADATA_KEY = "hitl";

function asRecord(value: unknown): Record<string, unknown> | null {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    return null;
  }
  return value as Record<string, unknown>;
}

function asString(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

/** Non-string entries and blanks are dropped; order is preserved. */
function asStringArray(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  const out: string[] = [];
  for (const entry of value) {
    const text = asString(entry);
    if (text) out.push(text);
  }
  return out;
}

function asConsequences(value: unknown): ConsequenceEntry[] {
  const record = asRecord(value);
  if (!record) return [];
  const out: ConsequenceEntry[] = [];
  for (const [label, text] of Object.entries(record)) {
    const body = asString(text);
    if (label.trim() && body) {
      out.push({ label: label.trim(), text: body });
    }
  }
  return out;
}

function parseJSONObject(raw: string | null | undefined): Record<string, unknown> | null {
  if (!raw || !raw.trim()) return null;
  try {
    return asRecord(JSON.parse(raw));
  } catch {
    return null;
  }
}

/**
 * Read the HITL ask off a loop's metadataJson. Returns null when the metadata
 * is absent, unparseable, or carries no `hitl` object.
 */
export function parseHITLAsk(
  metadataJson: string | null | undefined,
): HITLAsk | null {
  const ask = asRecord(parseJSONObject(metadataJson)?.[HITL_METADATA_KEY]);
  if (!ask) return null;
  return {
    question: asString(ask.question),
    options: asStringArray(ask.options),
    status: asString(ask.status).toLowerCase(),
    answer: asString(ask.answer),
    recommendation: asString(ask.recommendation),
    recommendedOption: asString(ask.recommendedOption),
    confidence: asString(ask.confidence),
    consequences: asConsequences(ask.consequences),
    askedAt: asString(ask.askedAt),
    answeredAt: asString(ask.answeredAt),
  };
}

/** internal/domain.LoopStatusAwaitingHuman — the status a blocking ask parks a loop in. */
const AWAITING_HUMAN_LOOP_STATUS = "awaiting_human";

/** True only while the loop is blocked on this ask. */
export function isAwaitingHuman(ask: HITLAsk | null): boolean {
  return ask?.status === "awaiting";
}

/**
 * True when the answer was stored but the loop never left awaiting_human.
 *
 * api.deliverHumanAnswer persists the answer and flips the loop to running in
 * two steps, so a failure between them strands the loop: the ask reads
 * "answered" and nothing will ever claim the loop again. On the successful path
 * the flip lands and the loop reads "running", so this stays false for the whole
 * normal answered window — only the stuck one matches.
 */
export function isResumeStalled(
  ask: HITLAsk | null,
  loopStatus: string | null | undefined,
): boolean {
  return (
    ask?.status === "answered" &&
    asString(loopStatus).toLowerCase() === AWAITING_HUMAN_LOOP_STATUS
  );
}

/**
 * A Planner escalation question ends with the criteria it fired, one
 * "- name (threshold): observed" line each — the same rows the criteria section
 * renders in full. Drop only those trailing lines, and only when the criterion
 * they name is actually being shown, so the question reads as the decision
 * while nothing leaves the card: an ask with no rendered criteria (every
 * non-Planner ask, and any load that failed) is returned untouched.
 */
export function questionWithoutRenderedCriteria(
  question: string,
  criteria: readonly FiredCriterion[],
): string {
  if (!question || criteria.length === 0) return question;
  const rendered = new Set(criteria.map((entry) => entry.criterion));
  const lines = question.split("\n");
  let end = lines.length;
  while (end > 1) {
    const line = lines[end - 1].trim();
    if (!line) {
      end -= 1;
      continue;
    }
    if (!line.startsWith("-")) break;
    const [name] = line.replace(/^-\s*/, "").split(/[\s(:]/, 1);
    if (!rendered.has(name)) break;
    end -= 1;
  }
  if (end === lines.length) return question;
  // The removed lines were what the trailing colon introduced; without them it
  // dangles.
  return lines.slice(0, end).join("\n").trimEnd().replace(/:$/, "");
}

function parseCriteria(value: unknown): FiredCriterion[] {
  if (!Array.isArray(value)) return [];
  const out: FiredCriterion[] = [];
  for (const entry of value) {
    const record = asRecord(entry);
    if (!record) continue;
    const criterion = asString(record.criterion);
    if (!criterion) continue;
    out.push({
      criterion,
      threshold: asString(record.threshold),
      observed: asString(record.observed),
      evidence: asStringArray(record.evidence),
    });
  }
  return out;
}

/**
 * Fired criteria from the newest planner.escalation event, or [] when the loop
 * has none (every non-Planner ask) or the payload is not the expected shape.
 *
 * The API serves entity events oldest-first, but the newest event is picked by
 * timestamp rather than position so a change of order cannot silently show a
 * superseded escalation.
 */
export function latestEscalationCriteria(
  items: readonly LoopEvent[] | null | undefined,
): FiredCriterion[] {
  if (!Array.isArray(items)) return [];
  let newest: LoopEvent | null = null;
  let newestAt = Number.NEGATIVE_INFINITY;
  for (const item of items) {
    if (item?.eventType !== PLANNER_ESCALATION_EVENT_TYPE) continue;
    const parsed = Date.parse(item.createdAt ?? "");
    const at = Number.isNaN(parsed) ? Number.NEGATIVE_INFINITY : parsed;
    if (newest === null || at >= newestAt) {
      newest = item;
      newestAt = at;
    }
  }
  if (!newest) return [];
  // payload is pre-parsed by the daemon, but it falls back to the raw string
  // when the stored JSON is invalid — re-parse payloadJson in that case.
  const payload =
    asRecord(newest.payload) ?? parseJSONObject(newest.payloadJson);
  return parseCriteria(payload?.criteria);
}
