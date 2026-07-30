import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { CopyButton } from "@/components/CopyButton";
import { StatusChip } from "@/components/StatusChip";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { fetchLoopEvents, respondToLoop } from "@/lib/api";
import { formatAge } from "@/lib/format";
import {
  isAwaitingHuman,
  latestEscalationCriteria,
  parseHITLAsk,
  questionWithoutRenderedCriteria,
  type FiredCriterion,
} from "@/lib/hitl";
import { useToast } from "@/lib/toast";

export type HumanDecisionCardProps = {
  /** Loop selector (seq or id) used in API paths. */
  selector: string;
  /** Loop id — the entity key the event log is indexed by. */
  loopId: string;
  /** Raw loop metadata JSON carrying the `hitl` ask. */
  metadataJson?: string | null;
  /** Called after a delivered answer so the page can refetch (use forceRefresh). */
  onResponded?: () => void | Promise<void>;
};

/**
 * Escalation detail is an enhancement, never a gate: the question and the
 * options come from loop metadata that is already loaded, so they render
 * before this resolves and stay usable when it fails.
 */
type DetailState =
  | { status: "loading" }
  | { status: "ready"; criteria: FiredCriterion[] }
  | { status: "error"; message: string };

function SectionLabel({ children }: { children: ReactNode }) {
  return (
    <div className="text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
      {children}
    </div>
  );
}

function Criteria({ criteria }: { criteria: FiredCriterion[] }) {
  return (
    <section className="flex flex-col gap-1">
      <SectionLabel>Why it stopped</SectionLabel>
      <div className="flex flex-col gap-1.5 rounded border border-[var(--border)] bg-[var(--bg)] p-2">
        {criteria.map((criterion, index) => (
          <div key={`${criterion.criterion}-${index}`}>
            <div className="flex flex-wrap items-baseline gap-x-2">
              <span className="mono text-[12px] text-[var(--text)]">
                {criterion.criterion}
              </span>
              {criterion.observed ? (
                <span className="mono text-[12px] text-[var(--text)]">
                  {criterion.observed}
                </span>
              ) : null}
              {criterion.threshold ? (
                <span className="text-[11px] text-[var(--text-muted)]">
                  vs <span className="mono">{criterion.threshold}</span>
                </span>
              ) : null}
            </div>
            {criterion.evidence.length ? (
              <ul className="m-0 mt-1 list-none border-l border-[var(--border)] p-0 pl-2">
                {criterion.evidence.map((line, evidenceIndex) => (
                  <li
                    key={`${line}-${evidenceIndex}`}
                    className="mono break-words text-[11px] leading-snug text-[var(--text-muted)]"
                  >
                    {line}
                  </li>
                ))}
              </ul>
            ) : null}
          </div>
        ))}
      </div>
    </section>
  );
}

/**
 * The blocking ask on a loop parked as awaiting_human: what is being decided,
 * why the loop stopped, what the agent recommends, and one button per answer.
 *
 * Renders nothing unless the loop metadata carries an ask still awaiting an
 * answer, so the page can mount it unconditionally.
 */
export function HumanDecisionCard({
  selector,
  loopId,
  metadataJson,
  onResponded,
}: HumanDecisionCardProps) {
  const toast = useToast();
  const ask = useMemo(() => parseHITLAsk(metadataJson), [metadataJson]);
  const awaiting = isAwaitingHuman(ask);

  const [detail, setDetail] = useState<DetailState>({ status: "loading" });
  const [pendingIndex, setPendingIndex] = useState<number | null>(null);

  useEffect(() => {
    if (!awaiting || !loopId) return;
    const controller = new AbortController();
    setDetail({ status: "loading" });

    void (async () => {
      try {
        const events = await fetchLoopEvents(loopId, controller.signal);
        if (controller.signal.aborted) return;
        setDetail({
          status: "ready",
          criteria: latestEscalationCriteria(events.items),
        });
      } catch (err) {
        if (controller.signal.aborted) return;
        if (err instanceof DOMException && err.name === "AbortError") return;
        if (err instanceof Error && err.name === "AbortError") return;
        setDetail({
          status: "error",
          message: err instanceof Error ? err.message : String(err),
        });
      }
    })();

    return () => controller.abort();
  }, [awaiting, loopId]);

  const respond = useCallback(
    async (answer: string, index: number) => {
      setPendingIndex(index);
      try {
        await respondToLoop(selector, answer);
        toast.success("Answer delivered");
        // The loop leaves awaiting_human on the refresh and this card unmounts.
        await onResponded?.();
      } catch (err) {
        toast.error(err instanceof Error ? err.message : String(err));
      } finally {
        setPendingIndex(null);
      }
    },
    [selector, toast, onResponded],
  );

  const respondRequest = useMemo(
    () =>
      `POST /api/v1/loops/${encodeURIComponent(selector)}/respond {"answer": "…"}`,
    [selector],
  );

  if (!ask || !awaiting) {
    return null;
  }

  const criteria = detail.status === "ready" ? detail.criteria : [];
  const showRecommendation =
    Boolean(ask.recommendation) ||
    Boolean(ask.recommendedOption) ||
    Boolean(ask.confidence);
  const busy = pendingIndex !== null;

  return (
    <Card
      title="Awaiting your decision"
      // Waiting on a human, not failed: warn accent, never danger.
      style={{ borderColor: "var(--warn)" }}
      actions={
        <div className="flex items-center gap-2">
          {ask.askedAt ? (
            <span className="text-[11px] text-[var(--text-muted)]">
              waiting {formatAge(ask.askedAt)}
            </span>
          ) : null}
          <StatusChip status="awaiting_human" />
        </div>
      }
    >
      <div className="flex flex-col gap-2.5">
        <p className="m-0 whitespace-pre-wrap break-words text-[14px] font-medium leading-snug text-[var(--text)]">
          {questionWithoutRenderedCriteria(ask.question, criteria) ||
            "(no question recorded)"}
        </p>

        {criteria.length > 0 ? (
          <Criteria criteria={criteria} />
        ) : detail.status === "loading" ? (
          <p className="m-0 text-[11px] text-[var(--text-muted)]">
            Loading decision detail…
          </p>
        ) : detail.status === "error" ? (
          <p
            className="m-0 text-[11px] text-[var(--text-muted)]"
            title={detail.message}
          >
            Decision detail could not be loaded — you can still answer.
          </p>
        ) : null}

        {showRecommendation ? (
          <section className="flex flex-col gap-1">
            <SectionLabel>Agent recommendation</SectionLabel>
            {ask.recommendation ? (
              <p className="m-0 whitespace-pre-wrap break-words text-[12px] leading-snug text-[var(--text-muted)]">
                {ask.recommendation}
              </p>
            ) : null}
            {ask.recommendedOption || ask.confidence ? (
              <p className="m-0 text-[11px] text-[var(--text-muted)]">
                {ask.recommendedOption ? (
                  <>
                    recommends{" "}
                    <span className="mono text-[var(--text)]">
                      {ask.recommendedOption}
                    </span>
                  </>
                ) : null}
                {ask.recommendedOption && ask.confidence ? " · " : null}
                {ask.confidence ? (
                  <>
                    confidence <span className="mono">{ask.confidence}</span>
                  </>
                ) : null}
              </p>
            ) : null}
          </section>
        ) : null}

        {/* Consequences explain what each option does, so they lead into the
            buttons rather than trailing the recommendation prose. */}
        {ask.consequences.length ? (
          <dl className="m-0 flex flex-col gap-1.5 border-t border-[var(--border)] pt-2">
            {ask.consequences.map((entry) => (
              <div key={entry.label}>
                <dt className="mono break-words text-[11px] text-[var(--text)]">
                  {entry.label}
                </dt>
                <dd className="m-0 text-[11px] leading-snug text-[var(--text-muted)]">
                  {entry.text}
                </dd>
              </div>
            ))}
          </dl>
        ) : null}

        {ask.options.length ? (
          <div className="flex flex-wrap items-center gap-1">
            {ask.options.map((option, index) => (
              <Button
                key={`${option}-${index}`}
                // The first option is the affirmative one.
                variant={index === 0 ? "default" : "ghost"}
                size="sm"
                disabled={busy}
                title={option}
                onClick={() => void respond(option, index)}
              >
                {pendingIndex === index ? `${option} …` : option}
              </Button>
            ))}
          </div>
        ) : (
          <div className="rounded border border-[var(--border)] bg-[var(--bg)] p-2">
            <div className="mb-1 flex items-center justify-between gap-2">
              <span className="text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
                Answer request
              </span>
              <CopyButton text={respondRequest} />
            </div>
            <p className="m-0 break-all mono text-[11px]">{respondRequest}</p>
            <p className="m-0 mt-1 text-[11px] text-[var(--text-muted)]">
              This ask carries no preset options — answer it with any text.
            </p>
          </div>
        )}
      </div>
    </Card>
  );
}
