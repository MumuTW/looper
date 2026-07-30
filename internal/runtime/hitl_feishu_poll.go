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
	"github.com/nexu-io/looper/internal/storage"
)

type feishuInboxEvent struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	RootID       string `json:"rootId"`
	SenderOpenID string `json:"senderOpenId"`
	Text         string `json:"text"`
	Value        struct {
		LoopSeq string `json:"loopSeq"`
		Answer  string `json:"answer"`
	} `json:"value"`
}

type feishuHITLPollDeps struct {
	loopByRoot    func(ctx contextType, rootID string) string
	loopBySeq     func(ctx contextType, seq int64) string
	deliverAnswer func(ctx contextType, loopID, answer string) error
	enqueueMessage func(ctx contextType, loopID, text string) error
	logWarn       func(msg string, fields map[string]any)
}

// pollFeishuHITLInboxOnce delivers answers among a batch of inbox events.
// Returns the count of successfully delivered events and a safe cursor value.
// The cursor advances to one before the first delivery-failed event ID so that
// failed events are retried on the next poll. Events intentionally skipped
// (wrong looper, empty text, unknown kind) do not block the cursor.
func pollFeishuHITLInboxOnce(ctx contextType, events []feishuInboxEvent, deps feishuHITLPollDeps, lastCursor int64) (delivered int, newCursor int64) {
	newCursor = lastCursor
	failedMin := int64(-1)

	for _, e := range events {
		loopID := ""
		value := ""
		var deliver func(contextType, string, string) error
		switch strings.TrimSpace(e.Kind) {
		case "message":
			text := strings.TrimSpace(e.Text)
			root := strings.TrimSpace(e.RootID)
			if text == "" || root == "" || deps.loopByRoot == nil || deps.enqueueMessage == nil {
				continue
			}
			loopID = deps.loopByRoot(ctx, root)
			value = text
			deliver = deps.enqueueMessage
		case "card_action":
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
			// Belongs to another looper or already resumed — does not block cursor.
			if e.ID > newCursor {
				newCursor = e.ID
			}
			continue
		}
		if err := deliver(ctx, loopID, value); err != nil {
			if deps.logWarn != nil {
				deps.logWarn("hitl feishu poll: deliver failed", map[string]any{"eventId": e.ID, "loopId": loopID, "kind": e.Kind, "error": err.Error()})
			}
			if failedMin < 0 || e.ID < failedMin {
				failedMin = e.ID
			}
			continue
		}
		delivered++
		if e.ID > newCursor {
			newCursor = e.ID
		}
	}

	if failedMin >= 0 && failedMin <= newCursor {
		newCursor = failedMin - 1
	}
	return delivered, newCursor
}

// feishuInboxCursor tracks the last inbox event id this daemon has consumed.
var feishuInboxCursor struct {
	mu sync.Mutex
	v  int64
}

var feishuInboxHTTPClient = &http.Client{Timeout: 10 * time.Second}

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
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			if err := deliverHITLAnswerToLoop(ctx, input.Repos, nowISO, loopID, answer); err != nil {
				return err
			}
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

	delivered, newCursor := pollFeishuHITLInboxOnce(ctx, events, deps, since)
	if newCursor > 0 {
		feishuInboxCursor.mu.Lock()
		if newCursor > feishuInboxCursor.v {
			feishuInboxCursor.v = newCursor
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

var _ = func(r *storage.Repositories) {
	_ = r.FeishuThreads
	_ = r.Loops
}
