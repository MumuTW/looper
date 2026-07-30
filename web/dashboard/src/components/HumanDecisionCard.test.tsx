import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { HumanDecisionCard } from "@/components/HumanDecisionCard";
import { ApiError } from "@/lib/api";
import { ToastProvider } from "@/lib/toast";

const fetchLoopEvents = vi.fn();
const respondToLoop = vi.fn();

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return {
    ...actual,
    fetchLoopEvents: (...args: unknown[]) => fetchLoopEvents(...args),
    respondToLoop: (...args: unknown[]) => respondToLoop(...args),
  };
});

/**
 * The two answers internal/planner.classifyEscalationAnswer accepts: it takes
 * the first token of the answer, so "stop" settles the Issue and anything else
 * (here "proceed") authorizes the spec.
 */
const PROCEED = "proceed: authorize Planner to write the spec";
const STOP = "stop: settle this Issue without a spec";

function metadata(overrides: Record<string, unknown> = {}): string {
  return JSON.stringify({
    hitl: {
      question:
        "Authorize Planner to write a spec for acme/looper#42? Repository exploration tripped 1 escalation criterion before spec authoring:",
      options: [PROCEED, STOP],
      status: "awaiting",
      recommendation: "Touches the API handler and three storage packages.",
      recommendedOption: PROCEED,
      confidence: "medium",
      consequences: {
        [PROCEED]: "Planner resumes in the same loop and authors the spec.",
      },
      askedAt: "2026-07-30T10:00:00.000Z",
      transport: "respond",
      ...overrides,
    },
  });
}

function escalationEvents() {
  return {
    entityType: "loop",
    entityId: "loop_1",
    items: [
      {
        id: "evt_1",
        eventType: "planner.escalation",
        payloadJson: "{}",
        createdAt: "2026-07-30T10:00:00.000Z",
        payload: {
          criteria: [
            {
              criterion: "blast_radius_files",
              threshold: "maxFilesTouched=8",
              observed: "23 files",
              evidence: ["internal/api/handler.go", "internal/storage/loops.go"],
            },
          ],
        },
      },
    ],
  };
}

function renderCard(
  props?: Partial<React.ComponentProps<typeof HumanDecisionCard>>,
) {
  const onResponded = vi.fn().mockResolvedValue(undefined);
  const view = render(
    <ToastProvider>
      <HumanDecisionCard
        selector="3491"
        loopId="loop_1"
        metadataJson={metadata()}
        onResponded={onResponded}
        {...props}
      />
    </ToastProvider>,
  );
  return { onResponded, view };
}

describe("HumanDecisionCard", () => {
  beforeEach(() => {
    fetchLoopEvents.mockReset();
    respondToLoop.mockReset();
    fetchLoopEvents.mockResolvedValue(escalationEvents());
    respondToLoop.mockResolvedValue({ id: "loop_1", status: "running" });
  });

  afterEach(() => {
    cleanup();
  });

  it("renders nothing when the loop is not awaiting a human", () => {
    const { view } = renderCard({
      metadataJson: metadata({ status: "consumed" }),
    });

    expect(view.container.textContent).toBe("");
    expect(fetchLoopEvents).not.toHaveBeenCalled();
  });

  it("renders nothing when metadata is absent or malformed", () => {
    const { view } = renderCard({ metadataJson: "{not json" });
    expect(view.container.textContent).toBe("");

    cleanup();
    const missing = renderCard({ metadataJson: null });
    expect(missing.view.container.textContent).toBe("");
    expect(fetchLoopEvents).not.toHaveBeenCalled();
  });

  it("shows the question, recommendation and options while detail is loading", () => {
    // Never resolves: the decision must be answerable before the event log is.
    fetchLoopEvents.mockReturnValue(new Promise(() => {}));
    renderCard();

    expect(screen.getByText(/Authorize Planner to write a spec/)).toBeTruthy();
    expect(screen.getByRole("button", { name: PROCEED })).toBeTruthy();
    expect(screen.getByRole("button", { name: STOP })).toBeTruthy();
    expect(
      screen.getByText("Touches the API handler and three storage packages."),
    ).toBeTruthy();
    expect(screen.getByText("Loading decision detail…")).toBeTruthy();
  });

  it("renders fired criteria from the newest planner escalation event", async () => {
    renderCard();

    expect(await screen.findByText("blast_radius_files")).toBeTruthy();
    expect(screen.getByText("23 files")).toBeTruthy();
    expect(screen.getByText("maxFilesTouched=8")).toBeTruthy();
    expect(screen.getByText("internal/api/handler.go")).toBeTruthy();
    expect(screen.getByText("internal/storage/loops.go")).toBeTruthy();
    expect(fetchLoopEvents).toHaveBeenCalledWith("loop_1", expect.anything());
  });

  it("drops question lines the criteria section repeats, and keeps them otherwise", async () => {
    const enumerated = metadata({
      question:
        "Authorize Planner to write a spec for acme/looper#42?\n- blast_radius_files (maxFilesTouched=8): 23 files",
    });
    renderCard({ metadataJson: enumerated });

    await screen.findByText("blast_radius_files");
    expect(
      screen.queryByText(/- blast_radius_files \(maxFilesTouched=8\)/),
    ).toBeNull();
    expect(
      screen.getByText("Authorize Planner to write a spec for acme/looper#42?"),
    ).toBeTruthy();

    // Without the criteria section the question is the only place they appear.
    cleanup();
    fetchLoopEvents.mockRejectedValue(new Error("events down"));
    renderCard({ metadataJson: enumerated });

    expect(
      await screen.findByText(/- blast_radius_files \(maxFilesTouched=8\)/),
    ).toBeTruthy();
  });

  // A loop with no planner escalation is the generic case, not an error.
  it("omits the criteria section when the loop has no escalation event", async () => {
    fetchLoopEvents.mockResolvedValue({
      entityType: "loop",
      entityId: "loop_1",
      items: [],
    });
    renderCard();

    await waitFor(() => {
      expect(screen.queryByText("Loading decision detail…")).toBeNull();
    });
    expect(screen.queryByText("Why it stopped")).toBeNull();
    expect(screen.getByRole("button", { name: PROCEED })).toBeTruthy();
  });

  it("stays fully answerable when the events fetch fails", async () => {
    fetchLoopEvents.mockRejectedValue(
      new ApiError("Events repository is not configured", {
        status: 500,
        code: "INTERNAL_ERROR",
      }),
    );
    renderCard();

    expect(
      await screen.findByText(/Decision detail could not be loaded/),
    ).toBeTruthy();
    expect(screen.getByText(/Authorize Planner to write a spec/)).toBeTruthy();
    const proceed = screen.getByRole("button", {
      name: PROCEED,
    }) as HTMLButtonElement;
    expect(proceed.disabled).toBe(false);
  });

  it("sends the option text verbatim and refreshes the loop", async () => {
    const { onResponded } = renderCard();

    fireEvent.click(screen.getByRole("button", { name: PROCEED }));

    await waitFor(() => {
      expect(respondToLoop).toHaveBeenCalledWith("3491", PROCEED);
    });
    expect(onResponded).toHaveBeenCalled();
  });

  it("sends the rejecting option verbatim", async () => {
    renderCard();

    fireEvent.click(screen.getByRole("button", { name: STOP }));

    await waitFor(() => {
      expect(respondToLoop).toHaveBeenCalledWith("3491", STOP);
    });
  });

  it("disables every option while responding and marks the pressed one", async () => {
    let release: (() => void) | null = null;
    respondToLoop.mockReturnValue(
      new Promise<void>((resolve) => {
        release = resolve;
      }),
    );
    renderCard();

    fireEvent.click(screen.getByRole("button", { name: PROCEED }));

    await waitFor(() => {
      expect(
        (screen.getByRole("button", { name: STOP }) as HTMLButtonElement)
          .disabled,
      ).toBe(true);
    });
    const pressed = screen.getByRole("button", {
      name: `${PROCEED} …`,
    }) as HTMLButtonElement;
    expect(pressed.disabled).toBe(true);

    release?.();
  });

  it("toasts and re-enables the options when responding fails", async () => {
    respondToLoop.mockRejectedValue(
      new ApiError("Loop loop_1 is not awaiting a human (status: running)", {
        status: 400,
        code: "VALIDATION_FAILED",
      }),
    );
    const { onResponded } = renderCard();

    fireEvent.click(screen.getByRole("button", { name: PROCEED }));

    expect(await screen.findByText(/is not awaiting a human/)).toBeTruthy();
    expect(
      (screen.getByRole("button", { name: PROCEED }) as HTMLButtonElement)
        .disabled,
    ).toBe(false);
    expect(onResponded).not.toHaveBeenCalled();
  });

  // A free-form ask (no preset options) must still say how to answer.
  it("shows the respond request when the ask carries no options", async () => {
    renderCard({ metadataJson: metadata({ options: [] }) });

    expect(
      await screen.findByText(
        'POST /api/v1/loops/3491/respond {"answer": "…"}',
      ),
    ).toBeTruthy();
  });
});
