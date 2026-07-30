// Package telegram is the Telegram Bot API intake transport: it turns a chat
// message into a forge Issue. It owns no workflow decisions — classification,
// routing, and planning stay with the existing Roles.
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
	"unicode/utf8"
)

const apiBase = "https://api.telegram.org"

// maxResponseBytes bounds a Bot API response read. getUpdates returns at most
// 100 updates and message text is capped at 4096 characters, so anything past
// this is a malfunctioning endpoint rather than data worth buffering.
const maxResponseBytes = 4 << 20

// maxMessageRunes is Telegram's sendMessage limit. Text is truncated to fit
// rather than rejected: a clipped confirmation is more useful than an error the
// sender never sees.
const maxMessageRunes = 4096

// Update is the subset of a Telegram update this transport acts on.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// Message is the subset of a Telegram message intake reads.
type Message struct {
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	From      *User  `json:"from"`
	Chat      *Chat  `json:"chat"`
}

// User is the sender; ID is what the intake allowlist matches on.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// Chat is the conversation a message belongs to.
type Chat struct {
	ID int64 `json:"id"`
}

// Client is a minimal Bot API client: poll updates in, plain messages out. It
// deliberately implements no streaming edits, media, or keyboards — the intake
// contract is text.
type Client struct {
	token string
	http  *http.Client
}

// NewClient returns a client for the given bot token. The token is never logged
// and never appears in an error, because the Bot API embeds it in the request
// path.
func NewClient(token string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{token: strings.TrimSpace(token), http: &http.Client{Timeout: timeout}}
}

// GetUpdates fetches updates after offset without long-polling. Telegram treats
// the offset as an acknowledgement of everything below it, so callers must only
// pass an offset past updates they have finished processing.
func (c *Client) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	form := url.Values{}
	form.Set("offset", strconv.FormatInt(offset, 10))
	form.Set("timeout", "0")
	form.Set("allowed_updates", `["message"]`)

	var parsed struct {
		Result []Update `json:"result"`
	}
	if err := c.call(ctx, "getUpdates", form, &parsed); err != nil {
		return nil, err
	}
	return parsed.Result, nil
}

// SendMessage posts text to a chat, truncating to Telegram's length limit.
func (c *Client) SendMessage(ctx context.Context, chatID, text string) error {
	form := url.Values{}
	form.Set("chat_id", chatID)
	form.Set("text", truncateRunes(text, maxMessageRunes))
	return c.call(ctx, "sendMessage", form, nil)
}

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit-1]) + "…"
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
		// A transport error embeds the request URL, and the URL embeds the bot
		// token. Report the method only.
		return fmt.Errorf("telegram %s: request failed", method)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	var envelope struct {
		OK          bool   `json:"ok"`
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
