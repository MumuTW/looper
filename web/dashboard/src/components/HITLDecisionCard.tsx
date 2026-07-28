import { useMemo, useState } from "react";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { respondLoop, type HITLAsk } from "@/lib/api";
import { useToast } from "@/lib/toast";

export type HITLDecisionCardProps = {
  /** Loop selector (seq or id) used in API paths. */
  selector: string;
  /** Current loop status; determines whether the card is interactive. */
  loopStatus: string;
  /** Parsed HITL ask from loop metadata. */
  ask: HITLAsk;
  /** Called after a successful respond so the page can refetch. */
  onMutated?: () => void | Promise<void>;
};

function titleForAsk(ask: HITLAsk): string {
  if (ask.role === "fixer") return "Fixer needs your call";
  if (ask.role === "worker") return "Worker needs your call";
  return "Needs your decision";
}

function confidenceStyle(confidence?: string): string {
  switch ((confidence || "").toLowerCase()) {
    case "high":
      return "border-[var(--ok)] text-[var(--ok)]";
    case "medium":
      return "border-[var(--border)] text-[var(--text)]";
    case "low":
      return "border-[var(--danger)] text-[var(--danger)]";
    default:
      return "border-[var(--border)] text-[var(--text-muted)]";
  }
}

export function HITLDecisionCard({
  selector,
  loopStatus,
  ask,
  onMutated,
}: HITLDecisionCardProps) {
  const toast = useToast();
  const [customAnswer, setCustomAnswer] = useState("");
  const [pendingOption, setPendingOption] = useState<string | null>(null);
  const [customPending, setCustomPending] = useState(false);
  const [inlineError, setInlineError] = useState<string | null>(null);

  const askStatus = (ask.status || "").toLowerCase();
  // Interactive respond is only valid while the loop is parked AND the ask has
  // not been answered/consumed yet. Missing ask status counts as awaiting.
  const interactive =
    loopStatus === "awaiting_human" &&
    (askStatus === "" || askStatus === "awaiting");

  const busy = pendingOption !== null || customPending;

  const submit = async (answer: string, source: "option" | "custom") => {
    const trimmed = answer.trim();
    if (!trimmed) {
      setInlineError("Answer cannot be empty");
      return;
    }
    setInlineError(null);
    if (source === "option") {
      setPendingOption(answer);
    } else {
      setCustomPending(true);
    }
    try {
      await respondLoop(selector, trimmed);
      toast.success("Answer delivered");
      setCustomAnswer("");
      await onMutated?.();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setInlineError(message);
      toast.error(message);
    } finally {
      setPendingOption(null);
      setCustomPending(false);
    }
  };

  const consequenceEntries = useMemo(
    () =>
      ask.consequences ? Object.entries(ask.consequences).filter(([, v]) => v) : [],
    [ask.consequences],
  );

  const title = titleForAsk(ask);

  // Read-only view when the ask has already been answered/consumed. We still
  // render a compact recap so operators can see what was decided without
  // digging into raw metadata.
  if (!interactive) {
    return (
      <Card
        title={title}
        actions={
          ask.status ? (
            <span className="text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
              {ask.status}
            </span>
          ) : null
        }
      >
        <div className="flex flex-col gap-2">
          <p className="m-0 text-[13px] leading-snug text-[var(--text)]">
            {ask.question}
          </p>
          {ask.answer ? (
            <div className="rounded border border-[var(--border)] bg-[var(--bg)] p-2">
              <div className="mb-1 text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
                Answer
              </div>
              <p className="m-0 whitespace-pre-wrap break-words text-[12px]">
                {ask.answer}
              </p>
            </div>
          ) : null}
          {ask.recommendation ? (
            <p className="m-0 text-[12px] text-[var(--text-muted)]">
              <span className="mr-1 font-medium text-[var(--text)]">
                Recommendation:
              </span>
              {ask.recommendation}
            </p>
          ) : null}
        </div>
      </Card>
    );
  }

  return (
    <Card
      title={title}
      actions={
        ask.confidence ? (
          <span
            className={`rounded border px-1.5 py-0.5 text-[10px] uppercase tracking-wide ${confidenceStyle(ask.confidence)}`}
            title="Agent-reported confidence"
          >
            confidence: {ask.confidence}
          </span>
        ) : null
      }
    >
      <div className="flex flex-col gap-3">
        {/* Question — the primary thing the human needs to answer. */}
        <p className="m-0 text-[14px] font-medium leading-snug text-[var(--text)]">
          {ask.question}
        </p>

        {ask.recommendation ? (
          <div className="rounded border border-[var(--border)] bg-[var(--bg)] p-2">
            <div className="mb-1 flex items-center justify-between gap-2">
              <span className="text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
                Recommendation
              </span>
              {ask.recommendedOption ? (
                <span className="mono text-[10px] text-[var(--text-muted)]">
                  recommended: {ask.recommendedOption}
                </span>
              ) : null}
            </div>
            <p className="m-0 whitespace-pre-wrap break-words text-[12px] leading-snug">
              {ask.recommendation}
            </p>
          </div>
        ) : null}

        {consequenceEntries.length > 0 ? (
          <div className="rounded border border-[var(--border)] bg-[var(--bg)] p-2">
            <div className="mb-1 text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
              Consequences
            </div>
            <dl className="m-0 flex flex-col gap-1">
              {consequenceEntries.map(([opt, effect]) => (
                <div
                  key={opt}
                  className="grid grid-cols-[minmax(0,1fr)_minmax(0,2fr)] gap-2 text-[12px] leading-snug"
                >
                  <dt className="m-0 mono break-words text-[var(--text)]">
                    {opt}
                  </dt>
                  <dd className="m-0 whitespace-pre-wrap break-words text-[var(--text-muted)]">
                    {effect}
                  </dd>
                </div>
              ))}
            </dl>
          </div>
        ) : null}

        {ask.options.length > 0 ? (
          <div className="flex flex-col gap-1.5">
            <span className="text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
              Pick an option
            </span>
            <div className="flex flex-wrap gap-1.5">
              {ask.options.map((opt) => {
                const isRecommended =
                  ask.recommendedOption != null &&
                  opt === ask.recommendedOption;
                const isPending = pendingOption === opt;
                return (
                  <Button
                    key={opt}
                    variant={isRecommended ? "default" : "ghost"}
                    size="md"
                    disabled={busy}
                    onClick={() => void submit(opt, "option")}
                    title={
                      isRecommended
                        ? `${opt} (agent recommendation)`
                        : opt
                    }
                    className="max-w-full whitespace-normal text-left"
                  >
                    <span className="min-w-0 break-words">
                      {isPending ? "Sending…" : opt}
                    </span>
                    {isRecommended && !isPending ? (
                      <span className="ml-1.5 shrink-0 text-[10px] opacity-80">
                        recommended
                      </span>
                    ) : null}
                  </Button>
                );
              })}
            </div>
          </div>
        ) : null}

        <div className="flex flex-col gap-1.5">
          <label
            htmlFor="hitl-custom-answer"
            className="text-[10px] uppercase tracking-wide text-[var(--text-muted)]"
          >
            Or write a custom answer
          </label>
          <textarea
            id="hitl-custom-answer"
            className="min-h-[72px] w-full resize-y rounded border border-[var(--border)] bg-[var(--bg)] px-2 py-1.5 text-[12px] text-[var(--text)] placeholder:text-[var(--text-muted)] focus:outline-none focus:ring-1 focus:ring-[var(--accent)]"
            placeholder="Custom guidance for the agent…"
            value={customAnswer}
            onChange={(e) => setCustomAnswer(e.target.value)}
            disabled={busy}
          />
          <div className="flex items-center justify-between gap-2">
            <span className="text-[11px] text-[var(--text-muted)]">
              {ask.headSha ? (
                <>
                  head <span className="mono">{ask.headSha.slice(0, 12)}</span>
                </>
              ) : null}
            </span>
            <Button
              variant="default"
              size="md"
              disabled={busy || customAnswer.trim().length === 0}
              onClick={() => void submit(customAnswer, "custom")}
            >
              {customPending ? "Sending…" : "Send answer"}
            </Button>
          </div>
        </div>

        {inlineError ? (
          <p className="m-0 text-[11px] text-[var(--danger)]">{inlineError}</p>
        ) : null}
      </div>
    </Card>
  );
}
