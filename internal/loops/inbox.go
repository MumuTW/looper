package loops

import (
	"encoding/json"

	"github.com/nexu-io/looper/internal/eventlog"
)

const humanInboxMetadataKey = "humanInbox"

// humanInboxCap bounds the pending human messages retained for a loop so a chatty
// thread can't grow metadata unbounded; oldest are dropped.
const humanInboxCap = 20

// HumanMessage is one free-text message a human sent into a loop's thread at any
// time — a follow-up question, a clarification, a new instruction — queued until
// the loop's next turn drains it and feeds it to the agent (same session). Unlike
// a button-click decision, a message does not by itself resolve a pending ask; the
// agent reads it in context and decides whether to proceed, answer, or re-ask.
type HumanMessage struct {
	ID   string `json:"id,omitempty"`
	At   string `json:"at"`
	Text string `json:"text"`
}

// ReadHumanInbox returns a loop's queued human messages in arrival order.
func ReadHumanInbox(metadataJSON *string) []HumanMessage {
	meta := parseMetadataObject(metadataJSON)
	raw, ok := meta[humanInboxMetadataKey]
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []HumanMessage
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil
	}
	return out
}

// AppendHumanMessage queues one human message (trimming to the most recent
// humanInboxCap), preserving all other metadata keys.
func AppendHumanMessage(metadataJSON *string, m HumanMessage) (string, error) {
	msgs := ReadHumanInbox(metadataJSON)
	for i := range msgs {
		if msgs[i].ID == "" {
			msgs[i].ID = eventlog.NewEventID("human")
		}
	}
	if m.ID == "" {
		m.ID = eventlog.NewEventID("human")
	}
	msgs = append(msgs, m)
	if len(msgs) > humanInboxCap {
		msgs = msgs[len(msgs)-humanInboxCap:]
	}
	return marshalWithHumanInbox(metadataJSON, msgs)
}

// ClearHumanInbox drops all queued human messages (called after the agent drains
// them in a turn).
func ClearHumanInbox(metadataJSON *string) (string, error) {
	return marshalWithHumanInbox(metadataJSON, nil)
}

// ClearHumanInboxMessages acknowledges exactly the messages that were included
// in an agent prompt. IDs make that acknowledgement independent of concurrent
// appends and capped eviction; an empty drained snapshot is deliberately a
// no-op. Legacy messages without IDs use their immutable payload only until the
// next append rewrites them with IDs.
func ClearHumanInboxMessages(metadataJSON *string, drained []HumanMessage) (string, error) {
	if len(drained) == 0 {
		if metadataJSON == nil {
			return "", nil
		}
		return *metadataJSON, nil
	}
	ids := make(map[string]struct{}, len(drained))
	legacy := make(map[string]int, len(drained))
	for _, message := range drained {
		if message.ID != "" {
			ids[message.ID] = struct{}{}
			continue
		}
		legacy[legacyHumanMessageKey(message)]++
	}
	remaining := make([]HumanMessage, 0, len(ReadHumanInbox(metadataJSON)))
	for _, message := range ReadHumanInbox(metadataJSON) {
		if message.ID != "" {
			if _, ok := ids[message.ID]; ok {
				continue
			}
		} else if key := legacyHumanMessageKey(message); legacy[key] > 0 {
			legacy[key]--
			continue
		}
		remaining = append(remaining, message)
	}
	return marshalWithHumanInbox(metadataJSON, remaining)
}

func legacyHumanMessageKey(message HumanMessage) string { return message.At + "\x00" + message.Text }

func marshalWithHumanInbox(metadataJSON *string, msgs []HumanMessage) (string, error) {
	meta, err := DecodeMetadataObjectForWrite(metadataJSON)
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		delete(meta, humanInboxMetadataKey)
	} else {
		encoded, err := json.Marshal(msgs)
		if err != nil {
			return "", err
		}
		var asSlice []any
		if err := json.Unmarshal(encoded, &asSlice); err != nil {
			return "", err
		}
		meta[humanInboxMetadataKey] = asSlice
	}
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
