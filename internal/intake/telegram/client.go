// Package telegram is the Telegram Bot API intake transport: it turns a chat
// message into a forge Issue and routes a reply back into a loop that is waiting
// on a human. It owns no workflow decisions — routing, classification, and
// planning stay with the existing Roles.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const apiBase = "https://api.telegram.org"

// maxResponseBytes bounds a Bot API response read. getUpdates returns at most 100
// updates and Telegram caps message text at 4096 chars, so anything past this is
// a malfunctioning endpoint rather than data we want to buffer.
const maxResponseBytes = 4 << 20

// Update is the subset of a Telegram update this transport acts on. Everything
// else in the payload is ignored.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// Message is the subset of a Telegram message intake reads.
type Message struct {
	MessageID      int64      `json:"message_id"`
	Text           string     `json:"text"`
	From           *User      `json:"from"`
	Chat           *Chat      `json:"chat"`
	ReplyToMessage *MessageID `json:"reply_to_message"`
}

// User is the sender of a message; ID is what the intake allowlist matches on.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// Chat is the conversation a message belongs to.
type Chat struct {
	ID int64 `json:"id"`
}

// MessageID references another message — used only to read a reply target.
type MessageID struct {
	MessageID int64 `json:"message_id"`
}

// Client is a minimal Bot API client: long-poll updates in, plain messages out.
// It deliberately implements no streaming edits, media, or keyboards — the intake
// contract is text.
type Client struct {
	token string
	http  *http.Client
}

// NewClient returns a client for the given bot token. The token is never logged
// and never included in an error message, because the Bot API embeds it in the
// request path.
func NewClient(token string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Client{token: strings.TrimSpace(token), http: &http.Client{Timeout: timeout}}
}

// GetUpdates long-polls for updates after offset. Telegram treats the offset as
// an acknowledgement of everything below it, so callers must only advance offset
// past an update they have finished processing.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSeconds int) ([]Update, error) {
	if timeoutSeconds < 0 {
		timeoutSeconds = 0
	}
	form := url.Values{}
	form.Set("offset", strconv.FormatInt(offset, 10))
	form.Set("timeout", strconv.Itoa(timeoutSeconds))
	form.Set("allowed_updates", `["message"]`)

	var parsed struct {
		Result []Update `json:"result"`
	}
	if err := c.call(ctx, "getUpdates", form, &parsed); err != nil {
		return nil, err
	}
	return parsed.Result, nil
}

// SendMessage posts text to a chat. replyTo threads the message under an earlier
// one when non-zero; Telegram tolerates a stale reply target by sending
// unthreaded, so a deleted ask message degrades rather than fails.
func (c *Client) SendMessage(ctx context.Context, chatID, text string, replyTo int64) (int64, error) {
	form := url.Values{}
	form.Set("chat_id", chatID)
	form.Set("text", text)
	if replyTo > 0 {
		form.Set("reply_to_message_id", strconv.FormatInt(replyTo, 10))
	}

	var parsed struct {
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := c.call(ctx, "sendMessage", form, &parsed); err != nil {
		return 0, err
	}
	return parsed.Result.MessageID, nil
}

func (c *Client) call(ctx context.Context, method string, form url.Values, out any) error {
	if c.token == "" {
		return fmt.Errorf("telegram %s: bot token is empty", method)
	}
	endpoint := fmt.Sprintf("%s/bot%s/%s", apiBase, c.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		// A transport error embeds the request URL — and the URL embeds the bot
		// token. Report the method only.
		return fmt.Errorf("telegram %s: request failed", method)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	var envelope struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("telegram %s: status %d with unparseable body", method, resp.StatusCode)
	}
	if !envelope.OK {
		return fmt.Errorf("telegram %s: status %d: %s", method, resp.StatusCode, strings.TrimSpace(envelope.Description))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("telegram %s: decode result: %w", method, err)
	}
	return nil
}
