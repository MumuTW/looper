package loops

import "encoding/json"

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
	msgs := append(ReadHumanInbox(metadataJSON), m)
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

// RemoveHumanMessages drops exactly the given messages (matched by At+Text) from the
// queued human messages, preserving any others — crucially, any that arrived AFTER the
// drained set was read (e.g. a message a human sent WHILE the agent was mid-run). Those
// must stay queued for a follow-up turn instead of being silently eaten when the run
// that never saw them clears the inbox. Duplicates are matched one-for-one.
func RemoveHumanMessages(metadataJSON *string, drained []HumanMessage) (string, error) {
	current := ReadHumanInbox(metadataJSON)
	if len(drained) == 0 || len(current) == 0 {
		return marshalWithHumanInbox(metadataJSON, current)
	}
	remove := make(map[string]int, len(drained))
	for _, m := range drained {
		remove[humanMessageKey(m)]++
	}
	kept := make([]HumanMessage, 0, len(current))
	for _, m := range current {
		k := humanMessageKey(m)
		if remove[k] > 0 {
			remove[k]--
			continue
		}
		kept = append(kept, m)
	}
	return marshalWithHumanInbox(metadataJSON, kept)
}

func humanMessageKey(m HumanMessage) string {
	return m.At + "\x00" + m.Text
}

const closedTaskAckMetadataKey = "hitlClosedTaskAckAt"

// ClosedTaskAckSent reports whether the one-time "this task is done, continue on
// the issue/PR" HITL ack has already been posted to this loop's thread. The marker
// lives in loop metadata so the ack fires at most once per loop even across daemon
// restarts — the in-memory inbox cursor resets on restart and re-reads old events,
// which would otherwise re-fire the ack every time (and a human replying again to a
// finished thread should not re-trigger it either).
func ClosedTaskAckSent(metadataJSON *string) bool {
	meta := parseMetadataObject(metadataJSON)
	s, _ := meta[closedTaskAckMetadataKey].(string)
	return s != ""
}

// MarkClosedTaskAckSent records that the closed-task HITL ack was posted at the
// given timestamp, preserving all other metadata keys.
func MarkClosedTaskAckSent(metadataJSON *string, at string) (string, error) {
	meta := parseMetadataObject(metadataJSON)
	meta[closedTaskAckMetadataKey] = at
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func marshalWithHumanInbox(metadataJSON *string, msgs []HumanMessage) (string, error) {
	meta := parseMetadataObject(metadataJSON)
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
