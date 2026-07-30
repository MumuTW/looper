import {
  useCallback,
  useEffect,
  useId,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { StatusChip } from "@/components/StatusChip";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { fetchLoopEvents, respondToLoop } from "@/lib/api";
import { formatAge } from "@/lib/format";
import {
  isAwaitingHuman,
  isResumeStalled,
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
  /** Current loop status: an answered ask still parked here is a stalled resume. */
  loopStatus?: string | null;
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

/**
 * The one answer in flight. Every control on the card disables while it is set,
 * and the control that started it says so.
 */
type Pending =
  | { kind: "option"; index: number }
  | { kind: "answer" }
  | { kind: "resume" };

const SECTION_LABEL_CLASS =
  "text-[10px] uppercase tracking-wide text-[var(--text-muted)]";

function SectionLabel({
  children,
  htmlFor,
}: {
  children: ReactNode;
  htmlFor?: string;
}) {
  if (htmlFor) {
    return (
      <label htmlFor={htmlFor} className={SECTION_LABEL_CLASS}>
        {children}
      </label>
    );
  }
  return <div className={SECTION_LABEL_CLASS}>{children}</div>;
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
 * Free-text answer, for the asks no button can answer: one that arrived with no
 * options at all, and a stalled resume whose recorded answer text is gone. Same
 * POST as the option buttons — the daemon rejects an empty answer, so an empty
 * box cannot be submitted.
 */
function AnswerComposer({
  hint,
  submitLabel,
  pendingLabel,
  busy,
  pending,
  onSubmit,
}: {
  hint: string;
  submitLabel: string;
  pendingLabel: string;
  /** Some answer is in flight — not necessarily this one. */
  busy: boolean;
  pending: boolean;
  onSubmit: (answer: string) => void;
}) {
  const inputId = useId();
  const [text, setText] = useState("");
  const answer = text.trim();

  // Left as typed on failure: the toast explains, the answer stays retryable.
  const submit = () => {
    if (!answer || busy) return;
    onSubmit(answer);
  };

  return (
    <section className="flex flex-col gap-1">
      <SectionLabel htmlFor={inputId}>Your answer</SectionLabel>
      <textarea
        id={inputId}
        className="min-h-14 w-full resize-y rounded border border-[var(--border)] bg-[var(--bg)] px-2 py-1 text-[12px] leading-snug text-[var(--text)] disabled:cursor-not-allowed disabled:opacity-60"
        value={text}
        disabled={busy}
        placeholder="Answer in your own words…"
        title="⌘/Ctrl + Enter sends"
        onChange={(event) => setText(event.currentTarget.value)}
        onKeyDown={(event) => {
          // Enter has to stay a newline in a textarea, so the modifier sends.
          if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
            event.preventDefault();
            submit();
          }
        }}
      />
      <div className="flex flex-wrap items-center justify-between gap-1.5">
        <span className="text-[11px] leading-snug text-[var(--text-muted)]">
          {hint}
        </span>
        <Button
          size="sm"
          disabled={!answer || busy}
          aria-keyshortcuts="Meta+Enter Control+Enter"
          onClick={submit}
        >
          {pending ? pendingLabel : submitLabel}
        </Button>
      </div>
    </section>
  );
}

/**
 * The blocking ask on a loop parked as awaiting_human: what is being decided,
 * why the loop stopped, what the agent recommends, and one button per answer.
 *
 * Renders nothing unless the loop metadata carries an ask still awaiting an
 * answer — or one whose answer was recorded without the loop ever resuming, the
 * one state where the operator needs this card most — so the page can mount it
 * unconditionally.
 */
export function HumanDecisionCard({
  selector,
  loopId,
  loopStatus,
  metadataJson,
  onResponded,
}: HumanDecisionCardProps) {
  const toast = useToast();
  const ask = useMemo(() => parseHITLAsk(metadataJson), [metadataJson]);
  const awaiting = isAwaitingHuman(ask, loopStatus);
  const stalled = isResumeStalled(ask, loopStatus);

  const [detail, setDetail] = useState<DetailState>({ status: "loading" });
  const [pending, setPending] = useState<Pending | null>(null);

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
  // askedAt identifies an ask even if the loop is re-escalated before polling
  // observes the intervening running state.
  }, [awaiting, loopId, ask?.askedAt]);

  const respond = useCallback(
    async (answer: string, marker: Pending, delivered: string) => {
      setPending(marker);
      try {
        await respondToLoop(selector, answer);
        toast.success(delivered);
        // The loop leaves awaiting_human on the refresh and this card unmounts.
        await onResponded?.();
      } catch (err) {
        toast.error(err instanceof Error ? err.message : String(err));
      } finally {
        setPending(null);
      }
    },
    [selector, toast, onResponded],
  );

  if (!ask || (!awaiting && !stalled)) {
    return null;
  }

  const busy = pending !== null;

  if (stalled) {
    // Re-POSTing the recorded answer replays both halves of /respond: the
    // handler only guards on the loop still being awaiting_human, so the write
    // lands again and the requeue it never reached is retried.
    const resume = () =>
      void respond(ask.answer, { kind: "resume" }, "Loop resumed");

    return (
      <Card
        title="Answer recorded — loop not resumed"
        // The wait is over and the requeue is not: a fault to clear, not a wait.
        style={{ borderColor: "var(--danger)" }}
        actions={
          <div className="flex items-center gap-2">
            {ask.answeredAt ? (
              <span className="text-[11px] text-[var(--text-muted)]">
                answered {formatAge(ask.answeredAt)} ago
              </span>
            ) : null}
            <StatusChip status="awaiting_human" />
          </div>
        }
      >
        <div className="flex flex-col gap-2.5">
          {/* The decision is settled, so the question is context now: two lines,
              the rest on hover. */}
          {ask.question ? (
            <p
              className="m-0 line-clamp-2 whitespace-pre-wrap break-words text-[12px] leading-snug text-[var(--text-muted)]"
              title={ask.question}
            >
              {ask.question}
            </p>
          ) : null}

          {ask.answer ? (
            <section className="flex flex-col gap-1">
              <SectionLabel>Your answer</SectionLabel>
              <p className="m-0 whitespace-pre-wrap break-words rounded border border-[var(--border)] bg-[var(--bg)] p-2 mono text-[12px] text-[var(--text)]">
                {ask.answer}
              </p>
            </section>
          ) : null}

          <p className="m-0 text-[11px] leading-snug text-[var(--text-muted)]">
            The answer was saved, but the loop never left{" "}
            <span className="mono">awaiting_human</span> — the resume did not go
            through, and nothing will pick the loop up until it does.
          </p>

          {ask.answer ? (
            <div className="flex flex-wrap items-center gap-1">
              <Button
                size="sm"
                disabled={busy}
                title="Re-sends the recorded answer and retries the requeue"
                onClick={resume}
              >
                {pending?.kind === "resume" ? "Resuming …" : "Resume loop"}
              </Button>
            </div>
          ) : (
            // No answer text survived, so there is nothing to re-send: the loop
            // stays answerable rather than becoming a dead end.
            <AnswerComposer
              hint="The recorded answer is missing — send it again to resume the loop."
              submitLabel="Resume loop"
              pendingLabel="Resuming …"
              busy={busy}
              pending={pending?.kind === "answer"}
              onSubmit={(answer) =>
                void respond(answer, { kind: "answer" }, "Loop resumed")
              }
            />
          )}
        </div>
      </Card>
    );
  }

  const criteria = detail.status === "ready" ? detail.criteria : [];
  const showRecommendation =
    Boolean(ask.recommendation) ||
    Boolean(ask.recommendedOption) ||
    Boolean(ask.confidence);

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
                onClick={() =>
                  void respond(
                    option,
                    { kind: "option", index },
                    "Answer delivered",
                  )
                }
              >
                {pending?.kind === "option" && pending.index === index
                  ? `${option} …`
                  : option}
              </Button>
            ))}
          </div>
        ) : (
          <AnswerComposer
            hint="This ask carries no preset options — your text is the answer."
            submitLabel="Send answer"
            pendingLabel="Sending …"
            busy={busy}
            pending={pending?.kind === "answer"}
            onSubmit={(answer) =>
              void respond(answer, { kind: "answer" }, "Answer delivered")
            }
          />
        )}
      </div>
    </Card>
  );
}
