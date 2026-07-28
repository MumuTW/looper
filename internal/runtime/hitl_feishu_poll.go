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
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/loops"
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
		LoopSeq     string `json:"loopSeq"`
		Answer      string `json:"answer"`
		ExecutionID string `json:"executionId"`
		AskedAt     string `json:"askedAt"`
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
	// loopAskGeneration loads the parked ask generation for a loop id so card
	// actions can reject stale tokens. Returns ok=false when no park exists.
	loopAskGeneration func(ctx contextType, loopID string) (executionID, askedAt string, ok bool)
	// deliverAnswer feeds a button-click decision into the shared HITL core.
	// executionID/askedAt bind the card to the park generation (validated under
	// the answer write lock).
	deliverAnswer func(ctx contextType, loopID, answer, executionID, askedAt string) error
	// enqueueMessage queues a free-text thread reply for the loop (conversational /
	// anytime), to be drained on the loop's next turn rather than treated as a final
	// answer.
	enqueueMessage func(ctx contextType, loopID, text string) error
	logWarn        func(msg string, fields map[string]any)
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
		switch strings.TrimSpace(e.Kind) {
		case "message":
			// A typed thread reply is conversational: queue it (question / new
			// instruction / an answer the agent will interpret), don't force it to
			// resolve the ask.
			text := strings.TrimSpace(e.Text)
			root := strings.TrimSpace(e.RootID)
			if text == "" || root == "" || deps.loopByRoot == nil || deps.enqueueMessage == nil {
				continue
			}
			loopID = deps.loopByRoot(ctx, root)
			if strings.TrimSpace(loopID) == "" {
				continue
			}
			if err := deps.enqueueMessage(ctx, loopID, text); err != nil {
				if deps.logWarn != nil {
					deps.logWarn("hitl feishu poll: deliver failed", map[string]any{"loopId": loopID, "kind": e.Kind, "error": err.Error()})
				}
				continue
			}
			delivered++
		case "card_action":
			// A button click is a clean decision → the shared answer path.
			ans := strings.TrimSpace(e.Value.Answer)
			seq, err := strconv.ParseInt(strings.TrimSpace(e.Value.LoopSeq), 10, 64)
			if ans == "" || err != nil || deps.loopBySeq == nil || deps.deliverAnswer == nil {
				continue
			}
			loopID = deps.loopBySeq(ctx, seq)
			if strings.TrimSpace(loopID) == "" {
				continue // belongs to another looper (or already resumed)
			}
			value = ans
			// Generation is re-checked inside deliverAnswer under the write lock.
			// Optional preflight avoids noisy deliver failures for obvious stales.
			if deps.loopAskGeneration != nil {
				parkExec, parkAsked, ok := deps.loopAskGeneration(ctx, loopID)
				if ok {
					park := loops.HITLAsk{ExecutionID: parkExec, AskedAt: parkAsked}
					if !loops.AskGenerationMatches(park, e.Value.ExecutionID, e.Value.AskedAt) {
						if deps.logWarn != nil {
							deps.logWarn("hitl feishu poll: stale ask generation", map[string]any{
								"loopId": loopID, "kind": e.Kind,
							})
						}
						continue
					}
				}
			}
			if err := deps.deliverAnswer(ctx, loopID, value, e.Value.ExecutionID, e.Value.AskedAt); err != nil {
				if deps.logWarn != nil {
					deps.logWarn("hitl feishu poll: deliver failed", map[string]any{"loopId": loopID, "kind": e.Kind, "error": err.Error()})
				}
				continue
			}
			delivered++
		default:
			continue
		}
	}
	return delivered, maxID
}

// feishuInboxCursor tracks the last inbox event id this daemon has consumed. In
// memory is sufficient: on restart it re-reads from 0 and delivery is idempotent.
var feishuInboxCursor struct {
	mu sync.Mutex
	v  int64
}

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

	feishuInboxCursor.mu.Lock()
	since := feishuInboxCursor.v
	feishuInboxCursor.mu.Unlock()

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
		loopAskGeneration: func(ctx contextType, loopID string) (executionID, askedAt string, ok bool) {
			loop, err := input.Repos.Loops.GetByID(ctx, loopID)
			if err != nil || loop == nil {
				return "", "", false
			}
			ask, found := loops.ReadHITLAsk(loop.MetadataJSON)
			if !found {
				return "", "", false
			}
			return ask.ExecutionID, ask.AskedAt, true
		},
		deliverAnswer: func(ctx contextType, loopID, answer, executionID, askedAt string) error {
			if err := deliverHITLAnswerToLoop(ctx, input.Repos, nowISO, loopID, answer, executionID, askedAt); err != nil {
				return err
			}
			// Mark the ask card resolved ("✅ 已选:X", brief preserved).
			if input.OnHITLAnswerDelivered != nil {
				input.OnHITLAnswerDelivered(ctx, loopID, answer)
			}
			return nil
		},
		enqueueMessage: func(ctx contextType, loopID, text string) error {
			return enqueueHumanMessageToLoop(ctx, input.Repos, nowISO, loopID, text)
		},
	}
	if input.Logger != nil {
		deps.logWarn = func(msg string, fields map[string]any) { input.Logger.Warn(msg, fields) }
	}

	delivered, maxID := pollFeishuHITLInboxOnce(ctx, events, deps)
	if maxID > 0 {
		feishuInboxCursor.mu.Lock()
		if maxID > feishuInboxCursor.v {
			feishuInboxCursor.v = maxID
		}
		feishuInboxCursor.mu.Unlock()
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
