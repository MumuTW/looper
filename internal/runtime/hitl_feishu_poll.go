package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
)

// feishuInboxEvent is one event from the shared Cloudflare inbox (GET /events).
type feishuInboxEvent struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"` // "message" | "card_action"
	RootID       string `json:"rootId"`
	SenderOpenID string `json:"senderOpenId"`
	Text         string `json:"text"`
	Value        struct {
		LoopSeq string `json:"loopSeq"`
		Answer  string `json:"answer"`
	} `json:"value"`
}

// feishuHITLPollDeps are the injected dependencies of the Feishu inbox poll lane.
type feishuHITLPollDeps struct {
	// loopByRoot maps a Feishu thread root message id to the loop that owns it
	// (this looper's local feishu_threads); "" when it belongs to another looper.
	loopByRoot func(ctx contextType, rootID string) string
	// loopBySeq maps a loop seq (from a card-action value) to a loop id; "" when
	// unknown to this looper.
	loopBySeq func(ctx contextType, seq int64) string
	// deliverAnswer feeds a button-click decision into the shared HITL core.
	deliverAnswer func(ctx contextType, loopID, answer string) error
	// enqueueMessage delivers a free-text thread reply to the loop (conversational /
	// anytime). It records the message and either reactivates the loop (so the agent
	// resumes to answer / continue — even a finished task, 线程永远可追问) or, when the
	// task cannot be safely resumed, posts the honest closed-task ack. It never
	// silently drops the reply.
	enqueueMessage func(ctx contextType, loopID, text string) error
	logWarn        func(msg string, fields map[string]any)
}

// loopFinishedForFollowup reports whether a loop has finished its work such that a
// further thread reply is a post-completion follow-up (§F). Only the "done well"
// states qualify — failed/abandoned loops may still be retried or acted on through
// other paths, so they are handled separately.
func loopFinishedForFollowup(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "done", "merged":
		return true
	default:
		return false
	}
}

// pollFeishuHITLInboxOnce delivers the answers among a batch of inbox events that
// belong to this looper's awaiting loops, self-selecting by thread root (typed
// replies) or loop seq (card-action clicks). Returns the highest event id seen so
// the caller can advance its cursor. Idempotent: an event whose loop is no longer
// awaiting is a no-op in deliverAnswer.
func pollFeishuHITLInboxOnce(ctx contextType, events []feishuInboxEvent, deps feishuHITLPollDeps) (delivered int, maxID int64) {
	for _, e := range events {
		if e.ID > maxID {
			maxID = e.ID
		}
		loopID := ""
		value := ""
		var deliver func(contextType, string, string) error
		switch strings.TrimSpace(e.Kind) {
		case "message":
			// A typed thread reply is conversational: deliver it (question / new
			// instruction / an answer the agent will interpret), don't force it to
			// resolve the ask. It is ALWAYS deliverable — even to a finished
			// (completed / merged) task (线程永远可追问): enqueueMessage records it and
			// either reactivates the loop so the agent resumes to answer / continue,
			// or, when the task cannot be safely resumed, posts the honest closed-task
			// ack. It is never silently dropped.
			text := strings.TrimSpace(e.Text)
			root := strings.TrimSpace(e.RootID)
			if text == "" || root == "" || deps.loopByRoot == nil || deps.enqueueMessage == nil {
				continue
			}
			loopID = deps.loopByRoot(ctx, root)
			value = text
			deliver = deps.enqueueMessage
		case "card_action":
			// A button click is a clean decision → the shared answer path.
			ans := strings.TrimSpace(e.Value.Answer)
			seq, err := strconv.ParseInt(strings.TrimSpace(e.Value.LoopSeq), 10, 64)
			if ans == "" || err != nil || deps.loopBySeq == nil {
				continue
			}
			loopID = deps.loopBySeq(ctx, seq)
			value = ans
			deliver = deps.deliverAnswer
		default:
			continue
		}
		if strings.TrimSpace(loopID) == "" {
			continue // belongs to another looper (or already resumed)
		}
		if err := deliver(ctx, loopID, value); err != nil {
			if deps.logWarn != nil {
				deps.logWarn("hitl feishu poll: deliver failed", map[string]any{"loopId": loopID, "kind": e.Kind, "error": err.Error()})
			}
			continue
		}
		delivered++
	}
	return delivered, maxID
}

// interruptRunningLoopAgent kills a loop's currently-running agent so a human's mid-run
// @bot is answered immediately (强操控 C1), instead of queued until the step finishes.
// The kill uses agent.HITLInterruptKillReason, which the planner turns into an
// "interrupted" run that re-dispatches to answer from the MAIN write-spec session. No-op
// when the loop has no running agent or the registry/repos aren't wired.
func interruptRunningLoopAgent(ctx contextType, input defaultSchedulerTickInput, loopID string) {
	if input.ActiveExecutions == nil || input.Repos == nil || input.Repos.AgentExecutions == nil {
		return
	}
	exec, err := input.Repos.AgentExecutions.GetLatestByLoopID(ctx, loopID)
	if err != nil || exec == nil {
		return
	}
	switch exec.Status {
	case "running", "started", "in_progress":
	default:
		return // no live agent to interrupt
	}
	runID := ""
	if exec.RunID != nil {
		runID = *exec.RunID
	}
	killed, err := input.ActiveExecutions.Kill(loopID, runID, exec.ID, agent.HITLInterruptKillReason)
	if err == nil && killed && input.Logger != nil {
		input.Logger.Info("hitl: interrupted running agent for a mid-run human message", map[string]any{"loopId": loopID})
	}
}

// feishuInboxCursorCounter names the persisted counter (counters table) tracking the
// last inbox event id this looper has consumed. Persisted — NOT in-memory — so a daemon
// restart does not reset it to 0 and re-read old events (which re-enqueued stale human
// messages, re-triggering spurious follow-up turns, and previously re-fired acks).
const feishuInboxCursorCounter = "feishu_inbox_cursor"

var feishuInboxHTTPClient = &http.Client{Timeout: 10 * time.Second}

// runFeishuHITLPoll polls the shared Cloudflare inbox once and delivers any
// answers for this looper's awaiting loops. Gated by the feishu transport +
// cf-inbox inbound; a no-op otherwise.
func runFeishuHITLPoll(ctx context.Context, input defaultSchedulerTickInput) {
	if input.Config == nil || !input.Config.HITL.Enabled || input.Repos == nil {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(input.Config.HITL.AnswerTransport), "feishu") {
		return
	}
	fs := input.Config.HITL.Feishu
	if fs == nil || !strings.EqualFold(strings.TrimSpace(fs.Inbound), "cf-inbox") {
		return
	}
	inboxURL := strings.TrimSpace(os.Getenv(strings.TrimSpace(fs.EventInboxURLEnv)))
	token := strings.TrimSpace(os.Getenv(strings.TrimSpace(fs.EventInboxTokenEnv)))
	if inboxURL == "" || token == "" {
		return
	}

	var since int64
	if input.Repos.Counters != nil {
		since, _ = input.Repos.Counters.Get(ctx, feishuInboxCursorCounter)
	}

	events, err := fetchFeishuInboxEvents(ctx, inboxURL, token, since)
	if err != nil {
		if input.Logger != nil {
			input.Logger.Warn("hitl feishu poll: fetch inbox failed", map[string]any{"error": err.Error()})
		}
		return
	}
	if len(events) == 0 {
		return
	}

	nowISO := eventlog.FormatJavaScriptISOString(input.Now().UTC())
	deps := feishuHITLPollDeps{
		loopByRoot: func(ctx contextType, rootID string) string {
			if input.Repos.FeishuThreads == nil {
				return ""
			}
			loopID, _ := input.Repos.FeishuThreads.LoopByRoot(ctx, rootID)
			return loopID
		},
		loopBySeq: func(ctx contextType, seq int64) string {
			loop, err := input.Repos.Loops.GetBySeq(ctx, seq)
			if err != nil || loop == nil {
				return ""
			}
			return loop.ID
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			if err := deliverHITLAnswerToLoop(ctx, input.Repos, nowISO, loopID, answer); err != nil {
				return err
			}
			// Mark the ask card resolved ("✅ 已选:X", brief preserved).
			if input.OnHITLAnswerDelivered != nil {
				input.OnHITLAnswerDelivered(ctx, loopID, answer)
			}
			return nil
		},
		enqueueMessage: func(ctx contextType, loopID, text string) error {
			// 强操控 (C1): if the loop's agent is running, interrupt it NOW so the human's
			// mid-run message is answered from the main session immediately, instead of
			// sitting in the inbox until the current step happens to finish.
			interruptRunningLoopAgent(ctx, input, loopID)
			ackClosed, err := enqueueHumanMessageToLoop(ctx, input.Repos, nowISO, loopID, text)
			if err != nil {
				return err
			}
			// The message landed on a finished task that could not be reactivated —
			// post the one honest "task is done, continue on the issue/PR" ack (§F
			// option B). Best-effort: the message is already recorded regardless.
			if ackClosed && input.PostTaskClosedFollowup != nil {
				if ackErr := input.PostTaskClosedFollowup(ctx, loopID); ackErr != nil && input.Logger != nil {
					input.Logger.Warn("hitl feishu poll: closed-task followup failed", map[string]any{"loopId": loopID, "error": ackErr.Error()})
				}
			}
			return nil
		},
	}
	if input.Logger != nil {
		deps.logWarn = func(msg string, fields map[string]any) { input.Logger.Warn(msg, fields) }
	}

	delivered, maxID := pollFeishuHITLInboxOnce(ctx, events, deps)
	if maxID > 0 && input.Repos.Counters != nil {
		if err := input.Repos.Counters.SetMax(ctx, feishuInboxCursorCounter, maxID); err != nil && input.Logger != nil {
			input.Logger.Warn("hitl feishu poll: persist cursor failed", map[string]any{"error": err.Error()})
		}
	}
	if delivered > 0 && input.Logger != nil {
		input.Logger.Info("hitl feishu: delivered human answers", map[string]any{"count": delivered})
	}
}

func fetchFeishuInboxEvents(ctx context.Context, inboxURL, token string, since int64) ([]feishuInboxEvent, error) {
	u, err := url.Parse(inboxURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("since", strconv.FormatInt(since, 10))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := feishuInboxHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("inbox responded with status %d", resp.StatusCode)
	}
	var parsed struct {
		OK     bool               `json:"ok"`
		Events []feishuInboxEvent `json:"events"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	return parsed.Events, nil
}

// storageReposForFeishuPoll is a compile-time assertion that the repos we rely on
// exist (keeps this file honest if the storage API changes).
var _ = func(r *storage.Repositories) {
	_ = r.FeishuThreads
	_ = r.Loops
}
