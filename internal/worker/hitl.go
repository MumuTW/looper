package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/labels"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// hitlSentinelRelPath is where an agent writes a mid-run question, relative to
// the worktree root. Mirrors synclo's afk ask sentinel.
const hitlSentinelRelPath = ".looper/ask.json"

// hitlPromptInstruction is appended to the worker prompt ONLY when hitl.enabled
// is true. It tells the agent how to pause and ask a human instead of guessing.
const hitlPromptInstruction = `

---
HUMAN-IN-THE-LOOP: You are trusted to make implementation decisions yourself. Being ABLE to form a reasonable recommendation is NOT a reason to ask a human — it is a reason to PROCEED. Do your homework (read the codebase and context), pick the best option, and carry on. State what you chose and why in your PR description so a human can course-correct in review — that review IS the checkpoint for reversible calls.

Escalate to a human — by writing a JSON file at .looper/ask.json in the repository root and then STOPPING — ONLY when one of these genuinely holds:
  1. You cannot form a confident recommendation: the options are a real toss-up, or the choice hinges on information only a human has (product intent, private context, an unstated requirement).
  2. The action is high-stakes or hard to reverse: destructive or data-losing, security- or privacy-sensitive, a public or contractual commitment, or it spends real money — a human should sign off even when you have an opinion.
  3. It is a direction or strategy call, not an implementation detail.
For everything else — naming, file contents, formats, defaults, structure, which reversible approach to take — DECIDE and proceed. Do not ask.

When you DO escalate, make it a decision brief the human can confirm in seconds (not raw research):
{
  "question": "<one concise question>",
  "options": ["<option 1>", "<option 2>"],
  "recommendation": "<1-2 sentences: what you found, what you'd do, and why>",
  "recommendedOption": "<the option you recommend, matching one of options>",
  "consequences": {"<option 1>": "<what happens if picked>", "<option 2>": "<what happens if picked>"},
  "confidence": "<high|medium|low>"
}
question + options are required; the rest are strongly encouraged (a bare question with no homework is a poor ask). Then STOP immediately without making further changes. A human will answer and you will be resumed in this same session with their decision.
---`

type hitlAsk struct {
	Question          string            `json:"question"`
	Options           []string          `json:"options"`
	Recommendation    string            `json:"recommendation,omitempty"`
	RecommendedOption string            `json:"recommendedOption,omitempty"`
	Consequences      map[string]string `json:"consequences,omitempty"`
	Confidence        string            `json:"confidence,omitempty"`
}

const hitlSentinelQuarantinePattern = "ask-*"

// maxAskSentinelBytes bounds a sentinel read; a decision brief is small, and
// an enormous file is not a decodable gate request. It also bounds the prefix
// duplicated into quarantine, so a compromised or looping agent cannot fill the
// daemon storage volume by repeatedly writing huge sentinels.
const maxAskSentinelBytes = 1 << 20

// maxQuarantineIncidentsPerLoop bounds how many malformed-sentinel incidents
// are retained per loop across all of its runs. Older incidents are pruned when
// a new one arrives, so a looping agent cannot grow quarantine without bound.
const maxQuarantineIncidentsPerLoop = 16

// consumeAskSentinel reads and removes the agent's ask sentinel from the
// worktree, if present. A missing sentinel is the ONLY no-question case:
// the sentinel is the agent's structured request for a human gate, so once
// the file exists, inability to read or decode it fails closed with the
// evidence quarantined for inspection — a truncated ask for a destructive
// decision must not let the worker continue to validation and publication.
// Consuming (deleting) a valid sentinel prevents the same question from
// re-suspending on resume.
//
// The sentinel path is resolved WITHOUT following symlinks: an agent-controlled
// ask.json symlink is treated as a protocol error and only the link itself is
// ever preserved or removed — the daemon never reads the target with its own
// privileges. Every protocol error carries a stable evidence hash so a resumed
// run can recognize the SAME sentinel a human already answered and consume it
// under human authority instead of re-asking forever when quarantine could not
// remove the original.
func consumeAskSentinel(worktreePath, quarantineRoot, loopID, runID string) (*hitlAsk, error) {
	if strings.TrimSpace(worktreePath) == "" {
		return nil, nil
	}
	path := filepath.Join(worktreePath, hitlSentinelRelPath)
	probe := probeAskSentinel(worktreePath, hitlSentinelRelPath)
	switch probe.kind {
	case askSentinelMissing:
		return nil, nil
	case askSentinelSymlink:
		cause := fmt.Errorf("ask sentinel is a symlink pointing at %q; a symlink is not a valid ask and its target is never read", probe.symlinkTarget)
		return nil, quarantineAskSentinel(quarantineRequest{
			path: path, root: quarantineRoot, loopID: loopID, runID: runID, cause: cause,
			evidenceKind: "symlink", symlinkTarget: probe.symlinkTarget,
		})
	case askSentinelIrregular:
		cause := fmt.Errorf("ask sentinel is not a regular file (mode %v)", probe.info.Mode())
		return nil, quarantineAskSentinel(quarantineRequest{
			path: path, root: quarantineRoot, loopID: loopID, runID: runID, cause: cause,
			evidenceKind: "irregular",
		})
	case askSentinelProbeErr:
		cause := fmt.Errorf("ask sentinel exists but cannot be probed: %w", probe.err)
		return nil, quarantineAskSentinel(quarantineRequest{
			path: path, root: quarantineRoot, loopID: loopID, runID: runID, cause: cause,
			evidenceKind: "unreadable",
		})
	}
	// askSentinelRegular. The probe confirmed (via no-follow stat) that the
	// sentinel is a regular file; probe.file is nil only when it could not be
	// opened for reading (e.g. mode 0), in which case the bytes are preserved by
	// renaming the whole file instead of reading it.
	originalSize := probe.info.Size()
	if probe.file == nil {
		cause := fmt.Errorf("ask sentinel exists but cannot be read: %w", probe.err)
		return nil, quarantineAskSentinel(quarantineRequest{
			path: path, root: quarantineRoot, loopID: loopID, runID: runID, cause: cause,
			evidenceKind: "unreadable", originalSize: originalSize, renameAllowed: true,
		})
	}
	defer probe.file.Close()
	if originalSize > maxAskSentinelBytes {
		raw := readAskSentinelPrefix(probe.file, maxAskSentinelBytes)
		cause := fmt.Errorf("ask sentinel is %d bytes (limit %d)", originalSize, maxAskSentinelBytes)
		return nil, quarantineAskSentinel(quarantineRequest{
			path: path, root: quarantineRoot, loopID: loopID, runID: runID, cause: cause,
			evidenceKind: "regular", evidenceBytes: raw, originalSize: originalSize,
		})
	}
	raw, readErr := io.ReadAll(io.LimitReader(probe.file, maxAskSentinelBytes+1))
	if readErr != nil {
		cause := fmt.Errorf("ask sentinel exists but cannot be read: %w", readErr)
		return nil, quarantineAskSentinel(quarantineRequest{
			path: path, root: quarantineRoot, loopID: loopID, runID: runID, cause: cause,
			evidenceKind: "unreadable", originalSize: originalSize, renameAllowed: true,
		})
	}
	if int64(len(raw)) > maxAskSentinelBytes {
		cause := fmt.Errorf("ask sentinel exceeds %d-byte limit while being read", maxAskSentinelBytes)
		return nil, quarantineAskSentinel(quarantineRequest{
			path: path, root: quarantineRoot, loopID: loopID, runID: runID, cause: cause,
			evidenceKind: "regular", evidenceBytes: raw[:maxAskSentinelBytes], originalSize: int64(len(raw)),
		})
	}
	var ask hitlAsk
	if err := json.Unmarshal(raw, &ask); err != nil {
		return nil, quarantineAskSentinel(quarantineRequest{
			path: path, root: quarantineRoot, loopID: loopID, runID: runID,
			cause:        fmt.Errorf("ask sentinel is not valid JSON: %w", err),
			evidenceKind: "regular", evidenceBytes: raw, originalSize: int64(len(raw)),
		})
	}
	if strings.TrimSpace(ask.Question) == "" {
		return nil, quarantineAskSentinel(quarantineRequest{
			path: path, root: quarantineRoot, loopID: loopID, runID: runID,
			cause:        errors.New("ask sentinel has no question"),
			evidenceKind: "regular", evidenceBytes: raw, originalSize: int64(len(raw)),
		})
	}
	if err := os.Remove(path); err != nil {
		return nil, quarantineAskSentinel(quarantineRequest{
			path: path, root: quarantineRoot, loopID: loopID, runID: runID,
			cause:        fmt.Errorf("decoded ask sentinel could not be consumed: %w", err),
			evidenceKind: "regular", evidenceBytes: raw, originalSize: int64(len(raw)),
		})
	}
	return &ask, nil
}

// readAskSentinelPrefix reads at most n bytes from a sentinel that is known to
// be oversized, for a bounded evidence fingerprint and quarantined prefix. A
// read error yields whatever was read (possibly empty); the caller still
// quarantines the incident with the size and hash of the prefix.
func readAskSentinelPrefix(file *os.File, n int) []byte {
	buf := make([]byte, 0, n)
	tmp := make([]byte, 32*1024)
	for len(buf) < n {
		max := n - len(buf)
		if max > len(tmp) {
			max = len(tmp)
		}
		nr, err := file.Read(tmp[:max])
		if nr > 0 {
			buf = append(buf, tmp[:nr]...)
		}
		if err != nil {
			break
		}
	}
	return buf
}

// askSentinelProtocolError is structured daemon evidence that the HITL sentinel
// path was exercised but its request could not be decoded or consumed safely.
// It is not action authority: detectHumanAsk turns it into the existing HITLAsk
// state, and only a human response authorizes the resumed agent's next action.
type askSentinelProtocolError struct {
	cause           error
	originalPath    string
	evidencePath    string
	originalRemoved bool
	quarantineErr   error
	// evidenceKind is "regular" | "symlink" | "irregular" | "unreadable"; it
	// drives how quarantine records the evidence without reading a symlink target.
	evidenceKind string
	// evidenceHash is the stable sha256 fingerprint of the evidence (kind plus
	// the bounded bytes read, or the symlink target string). detectHumanAsk
	// persists it on the synthetic HITLAsk so a resumed run can match it.
	evidenceHash string
	// symlinkTarget is the link target string for a symlink sentinel (recorded
	// for diagnosis; the target file is never read).
	symlinkTarget string
}

func (e *askSentinelProtocolError) Error() string {
	switch {
	case e.quarantineErr == nil:
		return fmt.Sprintf("%v; evidence quarantined at %s", e.cause, e.evidencePath)
	case e.evidencePath != "" && e.originalRemoved:
		return fmt.Sprintf("%v; evidence moved to %s but quarantine hardening failed: %v", e.cause, e.evidencePath, e.quarantineErr)
	case e.evidencePath != "":
		return fmt.Sprintf("%v; quarantine incomplete: %v (copy at %s; original remains at %s)", e.cause, e.quarantineErr, e.evidencePath, e.originalPath)
	default:
		return fmt.Sprintf("%v; quarantine failed: %v (evidence remains at %s)", e.cause, e.quarantineErr, e.originalPath)
	}
}

func (e *askSentinelProtocolError) Unwrap() error { return e.cause }

type askSentinelFileOps struct {
	rename func(string, string) error
	remove func(string) error
}

func (ops askSentinelFileOps) withDefaults() askSentinelFileOps {
	if ops.rename == nil {
		ops.rename = os.Rename
	}
	if ops.remove == nil {
		ops.remove = os.Remove
	}
	return ops
}

// quarantineRequest carries everything quarantineAskSentinel needs to record an
// incident without re-reading the worktree: the bounded bytes already read (for
// a regular sentinel), the symlink target string (for a symlink), or nothing
// (for an unreadable/irregular sentinel, recorded as metadata only). When
// renameAllowed is set the sentinel could not be read, so quarantine may rename
// the whole file to preserve its content without ever opening it.
type quarantineRequest struct {
	path          string
	root          string
	loopID        string
	runID         string
	cause         error
	ops           askSentinelFileOps
	evidenceKind  string
	evidenceBytes []byte
	originalSize  int64
	symlinkTarget string
	renameAllowed bool
}

// askSentinelEvidenceHash computes the stable fingerprint used as the persisted
// evidence identity. The kind is mixed in so a symlink target string can never
// collide with regular-file bytes that happen to match it. An unreadable
// sentinel (no bytes read) is fingerprinted by its size, so distinct sizes are
// distinct identities even when the bytes are unavailable.
func askSentinelEvidenceHash(kind string, bytes []byte, symlinkTarget string, originalSize int64) string {
	h := sha256.New()
	_, _ = io.WriteString(h, kind)
	_, _ = io.WriteString(h, "\n")
	switch kind {
	case "symlink":
		_, _ = io.WriteString(h, symlinkTarget)
	case "unreadable":
		_, _ = io.WriteString(h, strconv.FormatInt(originalSize, 10))
	default:
		_, _ = h.Write(bytes)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// quarantineAskSentinel records a malformed-sentinel incident in daemon-owned
// durable storage and removes the original from the worktree consume path. It
// never follows a symlink: a symlink sentinel is recorded as a metadata
// descriptor (target string only) and only the link itself is removed. Only a
// bounded prefix of a regular sentinel is ever duplicated into quarantine,
// alongside size/hash metadata, so an oversized sentinel cannot fill the
// daemon volume. A per-loop retention budget prunes the oldest incidents so a
// looping agent cannot grow quarantine without bound.
func quarantineAskSentinel(req quarantineRequest) error {
	incident := &askSentinelProtocolError{
		cause:         req.cause,
		originalPath:  req.path,
		evidenceKind:  req.evidenceKind,
		symlinkTarget: req.symlinkTarget,
		evidenceHash:  askSentinelEvidenceHash(req.evidenceKind, req.evidenceBytes, req.symlinkTarget, req.originalSize),
	}
	if strings.TrimSpace(req.root) == "" {
		incident.quarantineErr = errors.New("daemon quarantine root is not configured")
		return incident
	}
	loopDir := filepath.Join(req.root, durableQuarantineComponent(req.loopID, "unknown-loop"))
	parent := filepath.Join(loopDir, durableQuarantineComponent(req.runID, "unknown-run"))
	for _, directory := range []string{req.root, loopDir, parent} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			incident.quarantineErr = fmt.Errorf("create quarantine directory %s: %w", directory, err)
			return incident
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			incident.quarantineErr = fmt.Errorf("secure quarantine directory %s: %w", directory, err)
			return incident
		}
	}
	pruneQuarantineIncidents(loopDir, maxQuarantineIncidentsPerLoop-1)
	dir, err := os.MkdirTemp(parent, hitlSentinelQuarantinePattern)
	if err != nil {
		incident.quarantineErr = fmt.Errorf("create quarantine event directory: %w", err)
		return incident
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		incident.quarantineErr = fmt.Errorf("secure quarantine event directory: %w", err)
		return incident
	}
	destination := filepath.Join(dir, filepath.Base(req.path))
	incident.evidencePath = destination
	ops := req.ops.withDefaults()

	// An unreadable regular sentinel (no bytes captured) is preserved by moving
	// the whole file, which never reads its contents. This is only attempted
	// when the caller could not read the bytes; an oversized sentinel is never
	// renamed, so a huge file cannot be moved into quarantine unbounded.
	if req.renameAllowed {
		if err := ops.rename(req.path, destination); err == nil {
			incident.originalRemoved = true
			_ = os.Chmod(destination, 0o600)
			if sidecar, scErr := buildQuarantineSidecar(req, incident.evidenceHash); scErr == nil {
				_ = os.WriteFile(destination+".meta.json", sidecar, 0o600)
				_ = os.Chmod(destination+".meta.json", 0o600)
			}
			return incident
		}
		// Rename failed (e.g. cross-device); fall through to a descriptor + remove.
	}

	content, sidecar, writeErr := buildQuarantineEvidence(req, incident.evidenceHash)
	if writeErr != nil {
		incident.evidencePath = ""
		incident.quarantineErr = writeErr
		return incident
	}
	if err := writeQuarantineEvidence(destination, content, sidecar); err != nil {
		incident.evidencePath = ""
		incident.quarantineErr = err
		return incident
	}
	if err := ops.remove(req.path); err != nil {
		incident.quarantineErr = fmt.Errorf("remove original after quarantine copy: %w", err)
		return incident
	}
	incident.originalRemoved = true
	return incident
}

// buildQuarantineEvidence returns the bounded evidence bytes (or a metadata
// descriptor for non-regular sentinels) and the sidecar metadata to persist
// alongside. A symlink sentinel records its target string, never the target's
// contents.
func buildQuarantineEvidence(req quarantineRequest, hash string) ([]byte, []byte, error) {
	sidecar, err := buildQuarantineSidecar(req, hash)
	if err != nil {
		return nil, nil, err
	}
	switch req.evidenceKind {
	case "regular":
		return req.evidenceBytes, sidecar, nil
	case "unreadable":
		// No bytes were captured (the file could not be read and rename failed);
		// record a small descriptor so the incident is not an empty file.
		return appendQuarantineDescriptor(sidecar), sidecar, nil
	case "symlink", "irregular":
		return appendQuarantineDescriptor(sidecar), sidecar, nil
	default:
		return appendQuarantineDescriptor(sidecar), sidecar, nil
	}
}

// buildQuarantineSidecar renders the ask.meta.json sidecar recording the
// evidence kind, size/hash, and (for symlinks) the target string. For a renamed
// unreadable sentinel the evidence file holds the full original content and the
// sidecar's truncated flag is false.
func buildQuarantineSidecar(req quarantineRequest, hash string) ([]byte, error) {
	truncated := req.evidenceKind == "regular" && req.originalSize > int64(len(req.evidenceBytes))
	meta := map[string]any{
		"kind":         req.evidenceKind,
		"sha256":       hash,
		"originalSize": req.originalSize,
		"truncated":    truncated,
	}
	if req.evidenceKind == "symlink" {
		meta["target"] = req.symlinkTarget
	}
	sidecar, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode quarantine metadata: %w", err)
	}
	return sidecar, nil
}

func appendQuarantineDescriptor(sidecar []byte) []byte {
	var meta map[string]any
	if err := json.Unmarshal(sidecar, &meta); err != nil {
		meta = map[string]any{}
	}
	descriptor := map[string]any{
		"kind": meta["kind"],
		"note": "raw bytes not captured (symlink/irregular/unreadable sentinel); see ask.meta.json",
	}
	if target, ok := meta["target"]; ok {
		descriptor["target"] = target
	}
	raw, _ := json.MarshalIndent(descriptor, "", "  ")
	return raw
}

func writeQuarantineEvidence(destination string, content, sidecar []byte) error {
	if err := os.WriteFile(destination, content, 0o600); err != nil {
		return fmt.Errorf("write quarantined evidence: %w", err)
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		return fmt.Errorf("secure quarantined evidence: %w", err)
	}
	if len(sidecar) > 0 {
		if err := os.WriteFile(destination+".meta.json", sidecar, 0o600); err != nil {
			return fmt.Errorf("write quarantine metadata: %w", err)
		}
		_ = os.Chmod(destination+".meta.json", 0o600)
	}
	return nil
}

// pruneQuarantineIncidents removes the oldest ask-* incident directories under
// loopDir (across all runs) until at most keep remain. Failures are best-effort:
// pruning is a retention guard, not an authority, and must never prevent a new
// incident from being recorded.
func pruneQuarantineIncidents(loopDir string, keep int) {
	if keep < 0 {
		keep = 0
	}
	entries, err := os.ReadDir(loopDir)
	if err != nil {
		return
	}
	type incident struct {
		path  string
		mtime int64
	}
	var incidents []incident
	for _, runDir := range entries {
		if !runDir.IsDir() {
			continue
		}
		runPath := filepath.Join(loopDir, runDir.Name())
		events, err := os.ReadDir(runPath)
		if err != nil {
			continue
		}
		for _, ev := range events {
			if !ev.IsDir() {
				continue
			}
			info, err := ev.Info()
			if err != nil {
				continue
			}
			incidents = append(incidents, incident{path: filepath.Join(runPath, ev.Name()), mtime: info.ModTime().UnixNano()})
		}
	}
	if len(incidents) <= keep {
		return
	}
	sort.Slice(incidents, func(i, j int) bool { return incidents[i].mtime < incidents[j].mtime })
	for i := 0; i < len(incidents)-keep; i++ {
		_ = os.RemoveAll(incidents[i].path)
	}
}

func durableQuarantineComponent(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value != "" && value != "." && value != ".." && filepath.Base(value) == value {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%s-%x", fallback, sum[:8])
}

// awaitingHumanError is returned from the execute step when the agent asked a
// human mid-run. The step loop catches it and suspends the loop as
// awaiting_human instead of treating it as a failure.
type awaitingHumanError struct {
	question    string
	options     []string
	sessionID   string
	executionID string
	vendor      string
	// The agent's decision brief (optional) — carried through to the ask card.
	recommendation    string
	recommendedOption string
	consequences      map[string]string
	confidence        string
	// evidenceHash identifies the malformed sentinel a synthetic ask was raised
	// for (empty for a normal, decodable agent ask). Persisted on the HITLAsk so
	// a resumed run can consume the same sentinel under human authority.
	evidenceHash string
}

func (e *awaitingHumanError) Error() string { return "worker paused awaiting human decision" }

func asAwaitingHumanError(err error) (*awaitingHumanError, bool) {
	var typed *awaitingHumanError
	if errors.As(err, &typed) {
		return typed, true
	}
	return nil, false
}

// pendingHumanAnswer returns a resume prompt + native session id when the loop
// carries a human answer to a prior mid-run question. It is READ-ONLY: it does
// NOT mark the answer consumed. That matters because a resumed agent turn can
// fail or time out before completing — leaving the answer "answered" lets the
// retry re-read and re-deliver it instead of silently dropping the human's
// decision. The answer is flipped to "consumed" only once the turn completes,
// via markHumanAnswerConsumed. Returns empty strings when no answer is pending.
// Only called when hitl.enabled is true.
// pendingTakeoverResume returns the native session id (+ a continue prompt) a
// human drove during an interactive takeover that has since been handed back, so
// the daemon's next worker run resumes THAT session and sees their turns. Empty
// when no takeover resume is pending. Independent of hitl.enabled.
func (r *Runner) pendingTakeoverResume(ctx context.Context, loop *storage.LoopRecord, agentVendor string) (string, string) {
	tr, ok := loops.ReadTakeoverResume(loop.MetadataJSON)
	if !ok || strings.TrimSpace(tr.SessionID) == "" {
		return "", ""
	}
	prompt := strings.TrimSpace(tr.Prompt)
	if prompt == "" {
		prompt = "A human took this task's agent session over directly and has handed it back. Review the whole conversation so far — including their turns — and continue from where they left off; do not restart from scratch."
	}
	if r.repos == nil || r.repos.AgentExecutions == nil {
		return takeoverCheckpointRestartPrompt(), ""
	}
	if strings.TrimSpace(agentVendor) == "" {
		agentVendor = r.agentRuntime
	}
	execution, err := r.repos.AgentExecutions.GetLatestByLoopID(ctx, loop.ID)
	if err != nil || execution == nil || execution.NativeSessionID == nil || strings.TrimSpace(*execution.NativeSessionID) != strings.TrimSpace(tr.SessionID) || strings.TrimSpace(execution.Vendor) != strings.TrimSpace(agentVendor) {
		return takeoverCheckpointRestartPrompt(), ""
	}
	return prompt, strings.TrimSpace(tr.SessionID)
}

func takeoverCheckpointRestartPrompt() string {
	return "A human took this task over directly and has handed it back. The configured agent vendor no longer matches the captured session, so start a fresh session, inspect the current worktree and task state, and continue without trying to attach to the old conversation."
}

// latestNativeSessionID returns the loop's most recent captured agent session id,
// so a mailbox-driven turn can native-resume the SAME session and have the full
// conversation context. Empty when none is recorded. agentVendor is the run's
// execution-authority vendor (snapshot when present).
func (r *Runner) latestNativeSessionID(ctx context.Context, loopID, agentVendor string) string {
	if r.repos == nil || r.repos.AgentExecutions == nil {
		return ""
	}
	if strings.TrimSpace(agentVendor) == "" {
		agentVendor = r.agentRuntime
	}
	execution, err := r.repos.AgentExecutions.GetLatestByLoopID(ctx, loopID)
	if err != nil || execution == nil || execution.NativeSessionID == nil || strings.TrimSpace(execution.Vendor) != strings.TrimSpace(agentVendor) {
		return ""
	}
	return strings.TrimSpace(*execution.NativeSessionID)
}

// acknowledgePostTurnMetadata serializes every post-turn metadata mutation
// with free-text enqueue. A single fresh read and upsert avoids one mutation
// restoring metadata that an earlier mutation had just preserved.
func (r *Runner) acknowledgePostTurnMetadata(ctx context.Context, loop *storage.LoopRecord, consumed []loops.HumanMessage) {
	if r.repos == nil || r.repos.Loops == nil {
		return
	}
	unlock := loops.LockLoopRequeue(loop.ID)
	defer unlock()
	fresh, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil {
		return
	}
	meta := fresh.MetadataJSON
	changed := false
	if r.hitlEnabled {
		if ask, ok := loops.ReadHITLAsk(meta); ok && ask.Status == "answered" {
			ask.Status = "consumed"
			if updated, werr := loops.WriteHITLAsk(meta, ask); werr == nil {
				meta = &updated
				changed = true
			}
		}
		if len(consumed) > 0 {
			if updated, werr := loops.AcknowledgeHumanMessages(meta, consumed); werr == nil {
				meta = &updated
				changed = true
			}
		}
	}
	if _, ok := loops.ReadTakeoverResume(meta); ok {
		if updated, werr := loops.ClearTakeoverResume(meta); werr == nil {
			meta = &updated
			changed = true
		}
	}
	if !changed {
		return
	}
	fresh.MetadataJSON = meta
	fresh.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, *fresh); err == nil {
		loop.MetadataJSON = meta
	}
}

func (r *Runner) pendingHumanAnswer(ctx context.Context, loop *storage.LoopRecord, agentVendor string) (string, string) {
	ask, ok := r.readFreshHITLAsk(ctx, loop)
	if !ok || ask.Status != "answered" || strings.TrimSpace(ask.Answer) == "" {
		return "", ""
	}
	if strings.TrimSpace(agentVendor) == "" {
		agentVendor = r.agentRuntime
	}
	resumePrompt := fmt.Sprintf("A human answered the question you asked earlier (%q). Their decision: %s\nContinue the task using this decision; do not ask the same question again.", ask.Question, ask.Answer)
	if strings.TrimSpace(ask.Vendor) != strings.TrimSpace(agentVendor) {
		return resumePrompt + "\nThe configured agent vendor changed after the question was asked, so continue in a fresh session rather than trying to attach to the prior vendor's session.", ""
	}
	return resumePrompt, strings.TrimSpace(ask.SessionID)
}

// readFreshHITLAsk reads the loop's HITL ask from the freshest persisted record,
// falling back to the in-memory copy when the store is unavailable.
func (r *Runner) readFreshHITLAsk(ctx context.Context, loop *storage.LoopRecord) (loops.HITLAsk, bool) {
	meta := loop.MetadataJSON
	if r.repos != nil && r.repos.Loops != nil {
		if got, err := r.repos.Loops.GetByID(ctx, loop.ID); err == nil && got != nil {
			meta = got.MetadataJSON
		}
	}
	return loops.ReadHITLAsk(meta)
}

// detectHumanAsk consumes the agent's ask sentinel (if any) and, when present,
// returns a typed awaitingHumanError carrying the question, options, and the
// agent's native session id (so the run can resume the same session).
//
// When a sentinel fails to decode and the original could not be removed,
// quarantine is re-attempted on every resume. To keep that from re-asking
// forever, the synthetic ask persists the malformed sentinel's evidence hash.
// If the resumed run probes the SAME sentinel (same hash) and a human has
// already answered that ask, the human's response is the authority to consume
// the original under human authority and proceed — no new ask is raised. A
// genuinely new malformed sentinel (different hash) still raises a fresh ask.
func (r *Runner) detectHumanAsk(ctx context.Context, input stepInput, worktreePath, executionID string) (*awaitingHumanError, error) {
	ask, err := consumeAskSentinel(worktreePath, r.hitlQuarantineRoot, input.Loop.ID, input.Run.ID)
	if err != nil {
		var incident *askSentinelProtocolError
		evidenceHash := ""
		if errors.As(err, &incident) {
			evidenceHash = incident.evidenceHash
			if evidenceHash != "" {
				if existing, ok := r.readFreshHITLAsk(ctx, &input.Loop); ok &&
					existing.EvidenceHash == evidenceHash &&
					(existing.Status == "answered" || existing.Status == "consumed") {
					// The human already authorized this exact evidence. Consume the
					// original under human authority and proceed instead of looping.
					_ = os.Remove(filepath.Join(worktreePath, hitlSentinelRelPath))
					return nil, nil
				}
			}
		}
		// Sentinel presence is daemon-observed protocol evidence, not authority
		// for an inferred action. Persist a synthetic, answerable HITL ask so the
		// existing /respond path remains the only authority that resumes work.
		sessionID, vendor := r.latestAgentSession(ctx, input.Loop.ID)
		diagnostic := fmt.Sprintf("HITL ask sentinel failure for loop %s: %v", input.Loop.ID, err)
		return &awaitingHumanError{
			question:          "Looper could not decode the agent's human-decision request. After inspecting the evidence, should the agent regenerate the decision brief or continue without it?",
			options:           []string{"regenerate the decision brief", "continue without the original request"},
			sessionID:         sessionID,
			executionID:       executionID,
			vendor:            vendor,
			recommendation:    diagnostic,
			recommendedOption: "regenerate the decision brief",
			consequences: map[string]string{
				"regenerate the decision brief":         "resume the same agent context and require a new valid question before proceeding",
				"continue without the original request": "resume only because the human explicitly authorizes proceeding without that undecodable request",
			},
			confidence:   "low",
			evidenceHash: evidenceHash,
		}, nil
	}
	if ask == nil {
		return nil, nil
	}
	sessionID, vendor := r.latestAgentSession(ctx, input.Loop.ID)
	return &awaitingHumanError{
		question:          ask.Question,
		options:           ask.Options,
		sessionID:         sessionID,
		executionID:       executionID,
		vendor:            vendor,
		recommendation:    ask.Recommendation,
		recommendedOption: ask.RecommendedOption,
		consequences:      ask.Consequences,
		confidence:        ask.Confidence,
	}, nil
}

func (r *Runner) latestAgentSession(ctx context.Context, loopID string) (string, string) {
	if r.repos == nil || r.repos.AgentExecutions == nil {
		return "", ""
	}
	rec, err := r.repos.AgentExecutions.GetLatestByLoopID(ctx, loopID)
	if err != nil || rec == nil {
		return "", ""
	}
	sessionID := ""
	if rec.NativeSessionID != nil {
		sessionID = strings.TrimSpace(*rec.NativeSessionID)
	}
	return sessionID, rec.Vendor
}

// suspendForHuman parks a worker run as awaiting_human: it persists the ask
// state on the loop, transitions the loop to awaiting_human, cancels the claimed
// queue item (so /respond can requeue it), ends the run as interrupted
// (resumable from the checkpoint), and sends the ask-card. Only reached when
// hitl.enabled is true.
// abortSuspension unwinds a suspension that could not persist its ask: the
// run is ended as failed (createRunContext resumes failed/interrupted runs;
// a run left running would make the retried claim start a duplicate from
// prepare-work while the stale run stays active) and the cause is surfaced.
func (r *Runner) abortSuspension(ctx context.Context, run storage.RunRecord, checkpoint workerCheckpoint, cause error) (ProcessResult, error) {
	wrapped := fmt.Errorf("persist HITL ask before suspension: %w", cause)
	if _, err := r.completeRun(ctx, run, "failed", "", wrapped.Error(), checkpoint); err != nil {
		return ProcessResult{}, errors.Join(wrapped, err)
	}
	return ProcessResult{}, wrapped
}

func (r *Runner) suspendForHuman(ctx context.Context, input stepInput, run storage.RunRecord, checkpoint workerCheckpoint, awaiting *awaitingHumanError) (ProcessResult, error) {
	nowISO := r.nowISO()
	ask := loops.HITLAsk{
		Question:          awaiting.question,
		Options:           awaiting.options,
		SessionID:         awaiting.sessionID,
		ExecutionID:       awaiting.executionID,
		Vendor:            awaiting.vendor,
		Status:            "awaiting",
		AskedAt:           nowISO,
		Recommendation:    awaiting.recommendation,
		RecommendedOption: awaiting.recommendedOption,
		Consequences:      awaiting.consequences,
		Confidence:        awaiting.confidence,
		EvidenceHash:      awaiting.evidenceHash,
	}
	// Preflight the strict metadata decode before ANY GitHub side effects: a
	// malformed value would otherwise let deliverAskToGitHub publish a branch,
	// draft PR, question comment, and label whose ask can never be stored —
	// and queue retries would keep publishing unanswerable comments.
	if _, err := loops.DecodeMetadataObjectForWrite(input.Loop.MetadataJSON); err != nil {
		return r.abortSuspension(ctx, run, checkpoint, err)
	}
	// GitHub transport (default): post the question on a (draft) PR before parking,
	// so the ask metadata carries the PR + comment id the answer-poll lane needs.
	// Best-effort — the loop still parks awaiting_human if delivery fails.
	if r.hitlTransportGitHub() {
		if err := r.deliverAskToGitHub(ctx, input, &checkpoint, awaiting, &ask); err != nil && r.logger != nil {
			r.logger.Warn("worker HITL github ask delivery failed; loop parked awaiting human without a PR comment", map[string]any{
				"loopId": input.Loop.ID, "error": err.Error(),
			})
		}
	}
	var askWriteErr error
	// Hold the per-loop requeue mutex across the suspension's read-modify-write
	// for the same reason as post-turn acknowledgement: a message appended
	// concurrently by the enqueue path must not be overwritten by this upsert.
	unlockRequeue := loops.LockLoopRequeue(input.Loop.ID)
	_, updateErr := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) {
		meta, werr := loops.WriteHITLAsk(updated.MetadataJSON, ask)
		if werr != nil {
			// Without a stored ask the response paths can never resume this
			// loop; abort the whole suspension instead of parking it stranded.
			askWriteErr = werr
			return
		}
		// The agent re-asked after reading the queued human messages, so the
		// ones it actually saw are consumed — but a message that arrived while
		// it was running was never observed and stays queued for the resumed
		// turn.
		if meta, werr := loops.AcknowledgeHumanMessages(&meta, loops.ReadHumanInbox(input.Loop.MetadataJSON)); werr == nil {
			updated.MetadataJSON = &meta
		}
		updated.Status = "awaiting_human"
		updated.LastRunAt = stringPtr(nowISO)
		updated.NextRunAt = nil
	})
	unlockRequeue()
	if updateErr != nil {
		return ProcessResult{}, updateErr
	}
	if askWriteErr != nil {
		return r.abortSuspension(ctx, run, checkpoint, askWriteErr)
	}
	reason := "worker suspended awaiting human decision"
	if _, err := r.repos.Queue.CancelByLoop(ctx, input.Loop.ID, nowISO, &reason); err != nil {
		return ProcessResult{}, err
	}
	summary := "Awaiting human decision: " + awaiting.question
	if _, err := r.completeRun(ctx, run, "interrupted", summary, "", checkpoint); err != nil {
		return ProcessResult{}, err
	}
	if !r.hitlTransportGitHub() && r.hitlNotify != nil {
		notif := HITLAskNotification{
			ProjectID:         input.Project.ID,
			LoopID:            input.Loop.ID,
			LoopSeq:           input.Loop.Seq,
			RunID:             run.ID,
			Repo:              derefString(input.Loop.Repo),
			Title:             awaiting.question,
			Question:          awaiting.question,
			Options:           awaiting.options,
			Recommendation:    awaiting.recommendation,
			RecommendedOption: awaiting.recommendedOption,
			Consequences:      awaiting.consequences,
			Confidence:        awaiting.confidence,
		}
		// Source + trigger come from the loop's work metadata (issue #, url, author).
		if w := checkpoint.Work; w != nil {
			notif.TriggerLogin = w.TriggerLogin
			switch {
			case w.PRNumber > 0:
				notif.SourceType = "GitHub PR"
				notif.SourceRef = "#" + strconv.FormatInt(w.PRNumber, 10)
			case w.IssueNumber > 0:
				notif.SourceType = "GitHub Issue"
				notif.SourceRef = "#" + strconv.FormatInt(w.IssueNumber, 10)
				notif.SourceURL = w.IssueURL
			}
		}
		if err := r.hitlNotify(ctx, notif); err != nil && r.logger != nil {
			// The loop is already parked in awaiting_human; if the human is never
			// notified they must find it via the dashboard / API. Surface loudly so an
			// unconfigured or failing notifier can't silently strand a run.
			r.logger.Warn("worker HITL ask notification failed; loop parked awaiting human with no notification sent", map[string]any{
				"loopId": input.Loop.ID, "loopSeq": input.Loop.Seq, "runId": run.ID, "error": err.Error(),
			})
		}
	}
	return ProcessResult{LoopID: input.Loop.ID, RunID: run.ID, QueueItemID: input.QueueItem.ID, Status: "awaiting_human", Summary: summary}, nil
}

// hitlTransportGitHub reports whether the GitHub PR-comment ask transport is
// active. GitHub is the default when hitl is enabled and no transport is set.
func (r *Runner) hitlTransportGitHub() bool {
	t := strings.TrimSpace(strings.ToLower(r.hitlAnswerTransport))
	return t == "" || t == "github"
}

func (r *Runner) hitlAwaitingLabel() string {
	if l := strings.TrimSpace(r.hitlGitHub.AwaitingLabel); l != "" {
		return l
	}
	return labels.AwaitingHuman
}

// deliverAskToGitHub ensures a (draft) PR exists for the loop, posts the agent's
// question as a marked PR comment, labels the PR so the answer-poll lane finds
// it, and records the PR + comment id on the ask. Best-effort; returns an error
// the caller logs while still parking the loop awaiting_human.
func (r *Runner) deliverAskToGitHub(ctx context.Context, input stepInput, checkpoint *workerCheckpoint, awaiting *awaitingHumanError, ask *loops.HITLAsk) error {
	if checkpoint == nil {
		return fmt.Errorf("hitl github: worker checkpoint is required")
	}
	repo := derefString(input.Loop.Repo)
	if repo == "" && checkpoint.Work != nil {
		repo = checkpoint.Work.Repo
	}
	if strings.TrimSpace(repo) == "" {
		return fmt.Errorf("hitl github: no repo for loop %s", input.Loop.ID)
	}
	cwd := input.Project.RepoPath

	prNumber := int64(0)
	if checkpoint.PullRequest != nil && checkpoint.PullRequest.Number > 0 {
		prNumber = checkpoint.PullRequest.Number
	} else if input.Loop.PRNumber != nil && *input.Loop.PRNumber > 0 {
		prNumber = *input.Loop.PRNumber
	}
	// On a later ask (e.g. a multi-turn second question) the loop/checkpoint may not
	// carry the PR that an earlier ask already opened for this branch — find and
	// reuse it instead of trying to open a duplicate (which gh rejects).
	if prNumber == 0 && checkpoint.Worktree != nil && strings.TrimSpace(checkpoint.Worktree.Branch) != "" {
		base := strings.TrimSpace(checkpoint.Worktree.BaseBranch)
		var aliases []string
		if checkpoint.Work != nil {
			if base == "" {
				base = strings.TrimSpace(checkpoint.Work.BaseBranch)
			}
			aliases = buildWorkerBranchAliases(*checkpoint.Work, input.Loop.ID)
		}
		aliases = append(aliases, checkpoint.Worktree.Branch)
		if base == "" {
			base = "main"
		}
		if existing, err := r.findOpenPullRequestForBranch(ctx, repo, aliases, base, cwd); err == nil && existing != nil {
			prNumber = existing.Number
		}
	}
	if prNumber == 0 {
		created, err := r.ensureDraftPRForAsk(ctx, input, checkpoint, repo, cwd)
		if err != nil {
			return err
		}
		prNumber = created
	}
	// Persist the PR onto the loop so later asks + the answer-poll resolve it fast.
	if prNumber > 0 && (input.Loop.PRNumber == nil || *input.Loop.PRNumber != prNumber) {
		_, _ = r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) {
			updated.PRNumber = int64Ptr(prNumber)
		})
	}
	if prNumber == 0 {
		return fmt.Errorf("hitl github: could not resolve a PR for loop %s", input.Loop.ID)
	}

	body := buildGitHubAskComment(input.Loop.Seq, awaiting.question, awaiting.options, r.hitlGitHub.MentionLogins)
	disclosureAgent, disclosureModel := r.disclosureIdentity(input.Run)
	res, err := r.github.CreateIssueComment(ctx, IssueCommentInput{Repo: repo, IssueNumber: prNumber, Body: body, CWD: cwd, DisclosureAgent: disclosureAgent, DisclosureModel: disclosureModel})
	if err != nil {
		return err
	}
	if err := r.github.AddPullRequestLabels(ctx, PullRequestLabelsInput{Repo: repo, PRNumber: prNumber, Labels: []string{r.hitlAwaitingLabel()}, CWD: cwd}); err != nil && r.logger != nil {
		r.logger.Warn("hitl github: failed to add awaiting-human label", map[string]any{"repo": repo, "pr": prNumber, "error": err.Error()})
	}

	ask.Transport = "github"
	ask.PRNumber = prNumber
	ask.AskCommentID = res.ID
	return nil
}

// ensureDraftPRForAsk pushes the loop's WIP branch and opens a draft PR to carry
// the question. Requires committed WIP on the branch; returns an error when there
// is nothing to open a PR from.
func (r *Runner) ensureDraftPRForAsk(ctx context.Context, input stepInput, checkpoint *workerCheckpoint, repo, cwd string) (int64, error) {
	if checkpoint == nil || checkpoint.Worktree == nil || strings.TrimSpace(checkpoint.Worktree.Branch) == "" {
		return 0, fmt.Errorf("hitl github: no worktree branch to open a draft PR for loop %s", input.Loop.ID)
	}
	base := strings.TrimSpace(checkpoint.Worktree.BaseBranch)
	title := ""
	if checkpoint.Work != nil {
		if base == "" {
			base = strings.TrimSpace(checkpoint.Work.BaseBranch)
		}
		title = strings.TrimSpace(checkpoint.Work.Title)
	}
	if base == "" {
		base = "main"
	}
	if title == "" {
		title = "Looper WIP — awaiting a human decision"
	}
	worktreeRoot, err := workerWorktreeRoot(input.Project)
	if err != nil {
		return 0, err
	}
	if len(r.validationCommands) > 0 {
		validateInput := input
		validateInput.Checkpoint = *checkpoint
		validated, err := r.runValidateStep(ctx, validateInput)
		if err != nil {
			return 0, fmt.Errorf("hitl github: validation blocked draft publication: %w", err)
		}
		*checkpoint = validated
	}
	if err := r.git.Push(ctx, PushInput{RepoPath: cwd, WorktreeRoot: worktreeRoot, WorktreePath: checkpoint.Worktree.Path, Branch: checkpoint.Worktree.Branch, LocalHeadSHA: workerValidatedHeadSHA(*checkpoint, r.validationCommands), ProtectedBranches: compactStrings([]string{base})}); err != nil {
		return 0, err
	}
	disclosureAgent, disclosureModel := r.disclosureIdentity(input.Run)
	created, err := r.github.CreatePullRequest(ctx, CreatePullRequestInput{
		Repo:            repo,
		HeadBranch:      checkpoint.Worktree.Branch,
		BaseBranch:      base,
		Title:           title,
		Body:            "🚧 Draft opened by looper to ask a mid-run question — see the comment below. Not ready for review.",
		Draft:           true,
		CWD:             cwd,
		DisclosureAgent: disclosureAgent,
		DisclosureModel: disclosureModel,
	})
	if err != nil {
		return 0, err
	}
	return created.Number, nil
}

const hitlGitHubAskMarkerPrefix = "<!-- looper:hitl:ask v=1"

// buildGitHubAskComment renders the ask as a PR comment carrying a machine marker
// (so the poll lane finds it and never mistakes it for a human answer).
func buildGitHubAskComment(loopSeq int64, question string, options []string, mentionLogins []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s loop=%d -->\n", hitlGitHubAskMarkerPrefix, loopSeq)
	b.WriteString("🤔 **looper needs a decision to continue.**\n\n")
	b.WriteString(strings.TrimSpace(question))
	for _, o := range options {
		if o = strings.TrimSpace(o); o != "" {
			fmt.Fprintf(&b, "\n- %s", o)
		}
	}
	b.WriteString("\n\nReply to this comment with your choice — a letter, an option, or free-form guidance. I'll pick it up and continue on this PR.")
	if m := githubMentionLine(mentionLogins); m != "" {
		b.WriteString("\n\n" + m)
	}
	return b.String()
}

func githubMentionLine(logins []string) string {
	parts := make([]string, 0, len(logins))
	for _, l := range logins {
		if l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "@")); l != "" {
			parts = append(parts, "@"+l)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "/cc " + strings.Join(parts, " ")
}
