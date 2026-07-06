# Design: HITL transports — one core, pluggable channels

Status: draft for review · 2026-07 · supersedes the earlier "GitHub-only" framing

## 0. TL;DR

The mid-run HITL **core is transport-agnostic**: an agent pauses
(`awaiting_human`), a human answers, `deliverHumanAnswer` resumes the same loop.
*How* the question goes out and the answer comes back is a **pluggable transport,
chosen per deployment shape** — not a single global decision.

| deployment | default answer transport | extra infra |
| --- | --- | --- |
| generic OSS / external contributor | **GitHub PR comment** (+ `/respond` API) | none |
| self-hosted / server | `/respond` API or GitHub | none |
| a team that lives in Feishu (e.g. nexu) | **Feishu** notify + answer | a small event inbox (§5) |

**Open-source posture:** ship the core + the **GitHub transport + `/respond`** as
the batteries-included default (zero infra, everyone already has GitHub). Ship
**Feishu** as an *optional, config-gated* transport (secrets via env). The
Cloudflare event-inbox that makes a *shared* Feishu app work is a **deployment
recipe**, not part of core — nobody pulling looper is forced to run it.

## 1. Why transport-agnostic (and why not pick one globally)

- **Generic OSS users** don't use Feishu (it's regional) and won't stand up a
  relay. But every looper user already talks to **GitHub** (it's the forge). So a
  question asked as a **PR comment**, answered as a **PR reply**, needs *zero* new
  setup. That is the correct OSS default.
- **This team lives in Feishu** and rarely watches GitHub. HITL's defining need is
  a human *sees and answers promptly* while the agent sits blocked holding a
  worktree/slot. For them the answer must come back through **Feishu**.

Both are true at once. The core already routes any transport into
`deliverHumanAnswer`, so we don't choose — we let the **deployment** choose.

## 2. The core (already built, unchanged)

- `LoopStatusAwaitingHuman` + `running ↔ awaiting_human` transitions.
- `deliverHumanAnswer(loopID, answer)` — stores the answer, flips to `running`,
  requeues for resume.
- Worker resume — injects the answer, continues (native-session resume when the
  vendor session id was captured, else a fresh run over the same branch).

Every transport below is just an adapter that (a) delivers the question and (b)
feeds an answer into `deliverHumanAnswer`. The agent contract is unchanged: it
writes `.looper/ask.json {question, options}` and stops.

## 3. Transport A — GitHub PR comment (OSS default)

**Out:** on suspend, ensure the loop has a (draft) PR, post an ask comment with a
marker + question + options + `@mention`, add label `looper:awaiting-human`.

```
<!-- looper:hitl:ask v=1 loop=71 -->
🤔 looper needs a decision to continue
横向滚动条修 A(改 resize handle 边界)还是 B(整体 overflow-x)?
Reply with your choice — a letter, an option, or free-form guidance.
```

**In:** a poll lane over `awaiting_human` loops that have a PR: fetch issue
comments, take the first comment by a non-bot author (`author.login !=
CurrentUser().Login`) created after the ask → that body is the answer →
`deliverHumanAnswer` → drop the label.

Keyed on **loop state**, not provider discovery, so **Plane-sourced** loops are
followed too (sidesteps the fixer/coordinator plane-skip in `scheduler.go`).

Primitives all exist: `CreateIssueComment`, `ListIssueComments`,
`CurrentUserIdentity`, label ops. No new dependency, no inbound infra, works
behind NAT (poll only). Cost: a poll-interval of latency + a handful of
answer-detection heuristics.

## 4. Transport B — `/respond` API (self-host default)

Already shipped: `POST /api/v1/loops/{seq}/respond {"answer": "..."}`. A local UI
or curl answers. Zero infra; the natural fit for a single self-hosted daemon.

## 5. Transport C — Feishu (this team's default; optional in OSS)

Feishu is where this team lives, so the **question pushes to Feishu** (they see it
in seconds) and the **answer comes back through Feishu**. Two ways to get the
answer to a laptop that is behind NAT:

### C1. Per-person app + long-connection
Each person registers their own Feishu app, scans a QR, and the app **dials out**
to Feishu over a WebSocket long-connection — events land on *their* looper. No
public URL, no relay. Reference (proven, TS): `zarazhangrui/lark-coding-agent-bridge`.
We implement the same shape in Go via the official `oapi-sdk-go` long-connection.
Trade-off: each person sets up an app; a stateful connection to babysit
(reconnect on sleep/network change).

> A **shared** app cannot use long-connection: Feishu delivers each event to one
> connection, so N loopers would steal each other's answers. Shared app ⇒ C2.

### C2. Shared app + Cloudflare event-inbox (recommended for nexu)
One Feishu app (IT configures once; teammates do nothing). Its webhook posts to a
**dumb CF Worker "inbox"** that stores recent events (D1/KV) and exposes
`GET /events?since=<cursor>` behind a shared secret. Every looper **polls** the
inbox and keeps only events whose `chat/thread` matches its **local
`feishu_threads`** mapping (root → loop) → `deliverHumanAnswer`.

```
shared Feishu app ──webhook──▶ CF Worker (durable inbox: store + GET /events)
                                        ▲  (poll, outbound, NAT-friendly)
        each looper ───────────────────┘  self-selects events matching its
                                           own feishu_threads → deliverHumanAnswer
```

- The **ask still pushes straight to Feishu** — "human sees it in seconds" is
  unaffected. The inbox only carries the **answer** back; its poll interval adds
  latency to *looper picking up* the answer, not to the human noticing.
- No central registry: each looper self-selects via the `feishu_threads` table we
  already built. CF is a dumb inbox.
- Looper-side is **generic HTTP polling** (simpler than a long-connection — stateless,
  self-healing). The CF Worker is one implementation of the inbox; the same
  contract could run on a VPS/Lambda. **CF is a deployment detail, not core.**

Fits nexu: they already run CF Workers + D1 (`dashboard.nexu.space`) + the mini;
teammates get zero-setup.

## 6. GitHub read-only audit mirror (all Feishu paths)

When a HITL answer arrives over Feishu **and the loop has a PR**, looper posts a
**read-only** decision-log comment on the PR (marker `looper:decision-log`),
appending each Q/A via `UpdateIssueComment` so it stays one tidy comment. Asks
that happened **before** a PR existed are folded into the PR body / decision-log
when the PR is opened. Pure send — never read back, zero inbound. This gives the
Feishu path GitHub's one real advantage (a durable, code-adjacent audit trail)
for nearly free.

## 7. Configuration sketch

```jsonc
{
  "hitl": {
    "enabled": true,
    "answerTransport": "github",   // "github"(OSS default) | "respond" | "feishu"
    "github": {
      "awaitingLabel": "looper:awaiting-human",
      "mentionLogins": ["lefarcen"],
      "answerAuthors": []           // empty = any non-bot; else allowlist
    },
    "feishu": {                      // optional; secrets via env only
      "inbound": "cf-inbox",        // "long-connection" | "cf-inbox"
      "eventInboxUrlEnv": "LOOPER_FEISHU_INBOX_URL",
      "eventInboxTokenEnv": "LOOPER_FEISHU_INBOX_TOKEN",
      "auditMirrorToGitHub": true
    }
  },
  "notifications": { "webhook": { /* send-only Feishu (@, threads) */ } }
}
```

## 8. Open-source posture (answering "适合开源出去吗")

- **Core + GitHub transport + `/respond`** → yes, this *is* the OSS default:
  universal, zero infra, no regional dependency.
- **Feishu send-side notify** (threads, `@`, plain text) → open-source as optional,
  config-gated, env secrets. Already built this way.
- **Feishu inbound (C1/C2)** → open-source the *looper side* generically
  (long-connection client; or a generic "poll an event-inbox URL" client). The
  **shared-app + CF Worker** is documented as a **recipe/example**, run by teams
  that want it — not a core dependency, not the default.
- Result: a generic user gets working GitHub HITL out of the box; nexu turns on
  Feishu; nobody is forced to run Cloudflare or use Feishu.

## 9. Phasing

- **Phase 1 (OSS default, ship upstream first):** GitHub PR-comment transport
  (ask + `awaiting-human` label + answer-poll lane) → `deliverHumanAnswer`. Reuses
  all resume machinery. Clean, universal, infra-free. Plane loops included.
- **Phase 2 (nexu):** Feishu answer via **C2 (CF inbox)** — the dumb CF Worker +
  the looper poll lane + `feishu_threads` self-select. Plus the §6 audit mirror.
  (C1 long-connection remains an alternative for anyone who prefers no relay.)
- **Phase 3:** unify the answer-poll lane with the fixer for CI/conflict/review
  follow-up ("一路解决到底") and un-gate Plane PRs there. Independent value; not a
  prerequisite for HITL.

## 10. Blind spots this version corrects

1. The earlier draft made GitHub the *global* default off a premise ("devs watch
   GitHub") that is **false for this team**. Fixed: GitHub is the *OSS* default;
   this team defaults to Feishu. Deployment picks the transport.
2. "Feishu inbound is impractical" conflated the *shared-app long-connection*
   (真会抢事件) with the viable shapes (per-person long-connection, or shared-app
   + poll-inbox). NAT — not "where the webhook points" — is the real constraint;
   both viable shapes have the laptop reach **out**.
3. "GitHub HITL unlocks solve-to-the-end" over-coupled two orthogonal axes: the
   fixer fixes CI/conflicts on GitHub PRs regardless of where the HITL *answer*
   travels. Kept as an independent Phase 3.
4. Forcing a draft PR just to ask a question is heavy/premature when no reviewable
   code exists yet — a cost the Feishu path doesn't carry. Only the GitHub
   transport needs a PR to attach to.

## Open questions

1. First vs latest human comment as the GitHub answer (default: first).
2. GitHub answerers: any non-bot vs allowlist (default: any non-bot).
3. Ship Phase 1 (GitHub) upstream before building Phase 2 (nexu Feishu/CF), or run
   them in parallel since this team wants Feishu sooner?
