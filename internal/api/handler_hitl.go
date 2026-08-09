package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/MumuTW/looper/internal/agent"
	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/storage"
	pkgapi "github.com/MumuTW/looper/pkg/api"
)

type takeoverLoopResponse struct {
	LoopID        string `json:"loopId"`
	Vendor        string `json:"vendor,omitempty"`
	SessionID     string `json:"sessionId,omitempty"`
	WorktreePath  string `json:"worktreePath,omitempty"`
	Supported     bool   `json:"supported"`
	ResumeCommand string `json:"resumeCommand,omitempty"`
	Message       string `json:"message,omitempty"`
}

const feishuCallbackMaxPayloadBytes = 1 << 20

// takeoverLoop parks a loop for interactive human takeover and returns the exact
// command a human runs to resume the loop's agent session (same native session id,
// in the loop's worktree). The daemon's in-flight run is already stopped by the
// wired TakeoverLoop closure; here we only shape the response + resume command.
func (h *Handler) takeoverLoop(ctx context.Context, loopID string) (takeoverLoopResponse, error) {
	if h.context.TakeoverLoop == nil {
		return takeoverLoopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusServiceUnavailable, message: "Takeover is not available on this daemon"}
	}
	result, err := h.context.TakeoverLoop(ctx, loopID, "Taken over by a human via looper takeover")
	if err != nil {
		return takeoverLoopResponse{}, err
	}
	resp := takeoverLoopResponse{
		LoopID:       result.LoopID,
		Vendor:       result.Vendor,
		SessionID:    result.SessionID,
		WorktreePath: result.WorktreePath,
	}
	vendor := config.AgentVendor(strings.TrimSpace(result.Vendor))
	// Global agent.params (especially command/args) are owned by agent.vendor.
	// Role runs already filter via ParamsForRoleVendor; takeover must do the same
	// so a Claude role session is not handed a global Codex wrapper resume line.
	params := agent.ParamsForRoleVendor(h.context.Config.Agent.Params, h.context.Config.Agent.Vendor, vendor, nil)
	cmdLine, ok := agent.InteractiveResumeCommandLine(agent.ExecutorConfig{Vendor: vendor, ReasoningEffort: result.ReasoningEffort, Params: params}, result.WorktreePath, result.SessionID)
	resp.Supported = ok
	if ok {
		resp.ResumeCommand = cmdLine
	} else {
		resp.Message = "Interactive takeover needs a captured session id and a supported agent (codex/claude); the loop is parked in human_takeover — hand it back with `looper handback` to resume the daemon."
	}
	return resp, nil
}

// handbackLoop re-arms a taken-over loop so the daemon resumes it. It stamps the
// loop with the native session id the human drove (so the next worker run resumes
// THAT session and sees their turns), clears any queue item that survived the
// takeover race, then re-arms via the shared retry path.
func (h *Handler) handbackLoop(ctx context.Context, r *http.Request, loopID string) (any, error) {
	// Reject discard before any handback mutation. retryLoop is shared with
	// /retry, but handback must never wipe the human's interactive worktree edits
	// even if an API client includes discardWorktreeChanges on the handback body.
	if discardRequested, err := retryRequestRequestsDiscard(r); err != nil {
		return nil, err
	} else if discardRequested {
		return nil, apiError{
			code:    pkgapi.ErrorCodeValidationFailed,
			status:  http.StatusBadRequest,
			message: "discardWorktreeChanges is not allowed on handback; human interactive worktree edits must be preserved (retry with --discard-worktree-changes after handback if needed)",
		}
	}

	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Coordinator == nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}
	nowISO := eventlog.FormatJavaScriptISOString(h.now().UTC())
	err := storage.WithTransaction(ctx, services.Coordinator.DB(), nil, func(tx *sql.Tx) error {
		return loops.PrepareHandback(ctx, storage.NewRepositories(tx), loops.HandbackPreparationInput{LoopID: loopID, NowISO: nowISO})
	})
	if err != nil {
		return nil, mapLoopReactivationError(err, loopID)
	}
	// Handback reuses retry re-arm; fromHandback also rejects discard if body is
	// re-read after a client races another field in (defense in depth).
	return h.retryLoop(ctx, r, loopID, true)
}

// retryRequestRequestsDiscard peeks at a retry/handback JSON body for
// discardWorktreeChanges without consuming the request for a later retryLoop decode.
func retryRequestRequestsDiscard(r *http.Request) (bool, error) {
	if r == nil || r.Body == nil {
		return false, nil
	}
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxJSONMutationBodyBytes))
	_ = r.Body.Close()
	r.Body = io.NopCloser(strings.NewReader(string(raw)))
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return false, apiError{code: pkgapi.ErrorCodeRequestTooLarge, status: http.StatusRequestEntityTooLarge, message: fmt.Sprintf("Request body exceeds %d bytes", maxJSONMutationBodyBytes)}
		}
		return false, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Invalid retry request: %v", err)}
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return false, nil
	}
	var body retryLoopRequest
	if aerr := decodeStrictJSONValue(raw, &body); aerr != nil {
		return false, *aerr
	}
	return body.DiscardWorktreeChanges != nil && *body.DiscardWorktreeChanges, nil
}

type respondLoopRequest struct {
	Answer string `json:"answer"`
}

// respondToLoop delivers a human's answer to a loop suspended mid-run as
// awaiting_human: it validates the loop is awaiting a human, stores the answer
// on the loop's HITL metadata, and transitions the loop back to running (which
// requeues it and triggers a scheduler tick) so the next run resumes the same
// agent session with the answer. This is the testable core of the HITL bridge.
func (h *Handler) respondToLoop(ctx context.Context, r *http.Request, loopID string) (loopResponse, error) {
	var body respondLoopRequest
	if aerr := decodeJSONMutationBody(r, &body, false); aerr != nil {
		return loopResponse{}, *aerr
	}
	return h.deliverHumanAnswer(ctx, loopID, body.Answer)
}

// deliverHumanAnswer is the shared core of the HITL respond path: it validates
// the loop is awaiting_human, stores the answer on the loop's HITL metadata, and
// transitions the loop back to running (requeue + scheduler tick). Both the
// /respond API endpoint and the Feishu card-action receiver call it.
func (h *Handler) deliverHumanAnswer(ctx context.Context, loopID string, rawAnswer string) (loopResponse, error) {
	answer := strings.TrimSpace(rawAnswer)
	if answer == "" {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "respond requires a non-empty answer"}
	}

	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Coordinator == nil {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}
	nowISO := eventlog.FormatJavaScriptISOString(h.now().UTC())
	// Serialize with worker HITL correlation writes. Release before
	// mutateLoopStatus, which acquires the same requeue lock.
	result, err := func() (loops.HITLAnswerResult, error) {
		unlock := loops.LockLoopRequeue(loopID)
		defer unlock()
		return storage.WithTransactionValue(ctx, services.Coordinator.DB(), nil, func(tx *sql.Tx) (loops.HITLAnswerResult, error) {
			return loops.RecordHITLAnswer(ctx, storage.NewRepositories(tx), loops.HITLAnswerInput{
				LoopID: loopID, Answer: answer, NowISO: nowISO, RequireExistingAsk: true, ConsumeGateEvidence: true,
			})
		})
	}()
	if err != nil {
		if errors.Is(err, loops.ErrLoopNotFound) {
			return loopResponse{}, apiError{code: pkgapi.ErrorCodeLoopNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Loop not found: %s", loopID)}
		}
		var typed apiError
		if asAPIError(err, &typed) {
			return loopResponse{}, typed
		}
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if !result.Applied {
		if result.Loop.Status == string(domain.LoopStatusAwaitingHuman) {
			return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Loop %s has no readable HITL ask metadata", loopID)}
		}
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Loop %s is not awaiting a human (status: %s)", loopID, result.Loop.Status)}
	}

	// Transition awaiting_human -> running (requeues + triggers a scheduler tick)
	// so the next claim resumes the run with the stored answer.
	return h.mutateLoopStatus(ctx, loopID, domain.LoopStatusRunning)
}

type feishuCardActionEnvelope struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	// Token is the Feishu app Verification Token, echoed by Feishu in every event
	// and card-action callback. It is the shared secret that proves the request
	// originated from Feishu rather than an arbitrary client. (v1 card-action /
	// url_verification carry it at top level; v2 events carry it in header.token.)
	Token  string `json:"token"`
	Action struct {
		Tag   string          `json:"tag"`
		Value json.RawMessage `json:"value"`
	} `json:"action"`
	// v2 event envelope, used for im.message.receive_v1 (a human typing a free-text
	// reply in the ask thread).
	Header struct {
		EventType string `json:"event_type"`
		Token     string `json:"token"`
	} `json:"header"`
	Event struct {
		Message struct {
			MessageID   string `json:"message_id"`
			RootID      string `json:"root_id"`
			ThreadID    string `json:"thread_id"`
			ChatID      string `json:"chat_id"`
			MessageType string `json:"message_type"`
			Content     string `json:"content"`
		} `json:"message"`
		Sender struct {
			SenderType string `json:"sender_type"`
			SenderID   struct {
				OpenID string `json:"open_id"`
			} `json:"sender_id"`
		} `json:"sender"`
	} `json:"event"`
}

// handleFeishuCardActionRoute is the thin Feishu listener (receive side of the
// app-bot integration whose send side ships in the notifier). It receives a
// card-action callback when a human clicks an option button on an ask-card, maps
// the button value {loopSeq, answer} to the awaiting loop, and calls the shared
// respond logic in-process. It also answers Feishu's url_verification challenge.
// The whole route is gated by hitl.enabled.
//
// Transport choice: this uses the card-action WEBHOOK RECEIVER over looper's
// existing HTTP server rather than the larksuite long-connection WS SDK, to
// avoid a heavy new dependency. Point the Feishu app's event/card-callback URL
// at <daemon>/api/v1/hitl/feishu. Typed free-text replies (message events) are a
// documented future extension; button clicks are handled today.
func (h *Handler) handleFeishuCardActionRoute(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.Method != http.MethodPost {
		h.writeError(w, requestID, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: "Feishu card-action route requires POST"})
		return
	}
	var raw []byte
	if r.Body != nil {
		defer r.Body.Close()
		var readErr error
		raw, readErr = io.ReadAll(io.LimitReader(r.Body, feishuCallbackMaxPayloadBytes+1))
		if readErr != nil {
			h.writeError(w, requestID, internalServerError(readErr))
			return
		}
		if len(raw) > feishuCallbackMaxPayloadBytes {
			h.writeError(w, requestID, apiError{code: pkgapi.ErrorCodeRequestTooLarge, status: http.StatusRequestEntityTooLarge, message: fmt.Sprintf("Feishu callback body exceeds %d bytes", feishuCallbackMaxPayloadBytes)})
			return
		}
	}
	var envelope feishuCardActionEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		h.writeError(w, requestID, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "invalid Feishu callback body"})
		return
	}
	// Resolve the configured Feishu Verification Token (a shared secret Feishu
	// echoes in every callback). This is the ONLY origin check on this route, and
	// it is independent of authMode because Feishu's servers cannot send a looper
	// Bearer token.
	expectedToken := ""
	if envName := strings.TrimSpace(h.context.Config.Notifications.Webhook.VerificationTokenEnv); envName != "" {
		expectedToken = strings.TrimSpace(os.Getenv(envName))
	}
	// v1 card-action / url_verification carry the token at the top level; v2 events
	// carry it in header.token.
	presentedToken := strings.TrimSpace(envelope.Token)
	if presentedToken == "" {
		presentedToken = strings.TrimSpace(envelope.Header.Token)
	}
	tokenMatches := expectedToken != "" && subtle.ConstantTimeCompare([]byte(presentedToken), []byte(expectedToken)) == 1

	// Feishu URL-verification handshake: echo the challenge verbatim. When a token
	// is configured, require it to match even for the handshake. This path produces
	// no work and must succeed while admission is starting/stopping/degraded so
	// Feishu can register or revalidate the callback URL.
	if envelope.Type == "url_verification" {
		if expectedToken != "" && !tokenMatches {
			h.writeError(w, requestID, apiError{code: pkgapi.ErrorCodeUnauthorized, status: http.StatusUnauthorized, message: "Feishu verification token mismatch"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": envelope.Challenge})
		return
	}
	// Real card actions and thread replies mutate HITL state; require admission.
	if typed, denied := h.admissionMutationDenial(); denied {
		h.writeError(w, requestID, typed)
		return
	}
	if !h.context.Config.HITL.Enabled {
		h.writeError(w, requestID, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusForbidden, message: "hitl.enabled is false"})
		return
	}
	// A card action delivers human-authored text into an agent's coding session, so
	// it MUST be authenticated. Fail closed: require a configured, matching
	// verification token — otherwise any client that can reach this route could
	// inject arbitrary answers into any awaiting_human loop.
	if expectedToken == "" {
		h.writeError(w, requestID, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusForbidden, message: "Feishu card-action callback requires notifications.webhook.verificationTokenEnv to be configured"})
		return
	}
	if !tokenMatches {
		h.writeError(w, requestID, apiError{code: pkgapi.ErrorCodeUnauthorized, status: http.StatusUnauthorized, message: "Feishu verification token mismatch"})
		return
	}
	// A human typing a free-text reply in the ask thread arrives as a message event
	// rather than a card action — route it to the free-text handler.
	if envelope.Header.EventType == "im.message.receive_v1" {
		h.handleFeishuThreadReply(w, r, requestID, envelope)
		return
	}
	var value struct {
		LoopSeq string `json:"loopSeq"`
		Answer  string `json:"answer"`
	}
	if len(envelope.Action.Value) > 0 {
		_ = json.Unmarshal(envelope.Action.Value, &value)
	}
	loopSeq := strings.TrimSpace(value.LoopSeq)
	answer := strings.TrimSpace(value.Answer)
	if loopSeq == "" || answer == "" {
		h.writeError(w, requestID, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "card action must carry value.loopSeq and value.answer"})
		return
	}
	loop, err := h.resolveLoop(r.Context(), loopSeq)
	if err != nil {
		var typed apiError
		if !asAPIError(err, &typed) {
			typed = internalServerError(err)
		}
		h.writeError(w, requestID, typed)
		return
	}
	if _, err := h.deliverHumanAnswer(r.Context(), loop.ID, answer); err != nil {
		var typed apiError
		if !asAPIError(err, &typed) {
			typed = internalServerError(err)
		}
		h.writeError(w, requestID, typed)
		return
	}
	h.writeSuccess(w, requestID, map[string]any{"loopSeq": loopSeq, "delivered": true})
}

// handleFeishuThreadReply consumes a human's free-text reply typed in an ask
// thread (a Feishu im.message.receive_v1 event). It reverse-maps the thread root
// to the loop that asked and delivers the typed text as the answer — the lossless,
// type-anything counterpart to clicking an option button. Ordinary thread chatter
// (no matching awaiting_human loop) is ignored with 200 so Feishu stops retrying.
func (h *Handler) handleFeishuThreadReply(w http.ResponseWriter, r *http.Request, requestID string, envelope feishuCardActionEnvelope) {
	msg := envelope.Event.Message
	if msg.MessageType != "text" {
		h.writeSuccess(w, requestID, map[string]any{"delivered": false, "reason": "non-text message"})
		return
	}
	if !strings.EqualFold(strings.TrimSpace(envelope.Event.Sender.SenderType), "user") {
		h.writeSuccess(w, requestID, map[string]any{"delivered": false, "reason": "non-human sender"})
		return
	}
	rootID := strings.TrimSpace(msg.RootID)
	if rootID == "" {
		rootID = strings.TrimSpace(msg.ThreadID)
	}
	if rootID == "" {
		h.writeSuccess(w, requestID, map[string]any{"delivered": false, "reason": "not a thread reply"})
		return
	}
	var textContent struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(msg.Content), &textContent)
	answer := strings.TrimSpace(textContent.Text)
	if answer == "" {
		h.writeSuccess(w, requestID, map[string]any{"delivered": false, "reason": "empty text"})
		return
	}
	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Repositories.FeishuThreads == nil {
		h.writeSuccess(w, requestID, map[string]any{"delivered": false, "reason": "thread mapping unavailable"})
		return
	}
	loopID, err := services.Repositories.FeishuThreads.LoopByRoot(r.Context(), rootID)
	if err != nil {
		h.writeError(w, requestID, internalServerError(err))
		return
	}
	if strings.TrimSpace(loopID) == "" {
		h.writeSuccess(w, requestID, map[string]any{"delivered": false, "reason": "no loop for thread"})
		return
	}
	// Bot posts are already rejected by the sender_type gate above.
	// deliverHumanAnswer accepts only an awaiting_human loop, so this drops
	// replies after the loop resumed and duplicate Feishu retries.
	if _, err := h.deliverHumanAnswer(r.Context(), loopID, answer); err != nil {
		var typed apiError
		if asAPIError(err, &typed) {
			switch typed.status {
			case http.StatusBadRequest:
				h.writeSuccess(w, requestID, map[string]any{"loopId": loopID, "delivered": false, "reason": "loop not awaiting a human"})
				return
			case http.StatusNotFound:
				h.writeSuccess(w, requestID, map[string]any{"loopId": loopID, "delivered": false, "reason": "loop no longer exists"})
				return
			}
		}
		if !asAPIError(err, &typed) {
			typed = internalServerError(err)
		}
		h.writeError(w, requestID, typed)
		return
	}
	h.writeSuccess(w, requestID, map[string]any{"loopId": loopID, "delivered": true})
}
