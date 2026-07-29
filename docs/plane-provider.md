# Plane provider + Feishu HITL setup

This guide shows how to run Looper with [Plane](https://plane.so) as the **task source** while GitHub stays the **code forge**, and how to route human-in-the-loop (HITL) notifications to a Feishu webhook.

## What it is

Plane is a task tracker: it owns work-items (issues), labels, comments, and assignees, but it has no pull requests, diffs, or code reviews. The `plane` provider splits those two concerns:

- **Issue side → Plane.** Planner and Worker read work-items directly from a Plane project (filtered by a trigger label) via Plane's REST API. No redundant GitHub issue is created.
- **Code side → GitHub.** Pull requests are opened, reviewed, and merged on the project's GitHub `repo`, exactly like a normal GitHub project. This still requires an authenticated `gh`.

Concretely, for a `plane` project Looper maps each Plane work-item onto its internal issue type:

| Looper issue field | Plane work-item source |
| --- | --- |
| `Number` | `sequence_id` (per-project integer) |
| `Title` | `name` |
| `Body` | `description_html` (HTML stripped to text) |
| `Labels` | label UUIDs resolved to names |
| `Assignees` | assignee UUIDs |
| `URL` | Plane work-item web page |

Provider capabilities: issues, labels, comments, and assignees are supported; pull requests, diffs, and native reviews are **not** (they are delegated to GitHub). Review discovery is by label, review publishing is comment-only, and issue discovery is by polling.

## Setting it up

There is no generator for a Plane config. `looper bootstrap --provider plane`, and every flag it took, was removed with the old CLI. Define the provider and the complete project together in the config file's `providers` and `projects` sections, then restart `looperd`. Do not pre-register the path through `looper project add`, the dashboard, or the project API: that creates an API-managed record, and a later config entry with the same id conflicts with it and prevents daemon startup.

Run `looper init` for a starter config, then add the sections below by hand:

| Value | Config path | Notes |
| --- | --- | --- |
| Plane workspace slug | `providers[].workspace` | required |
| Plane project UUID | `providers[].projectId` | required |
| Plane REST API base | `providers[].baseUrl` | e.g. `https://plane.example.com/api/v1` |
| Env var holding the Plane API key | `providers[].tokenEnv` | conventionally `PLANE_API_KEY`; never store the key itself |
| GitHub code repo (`owner/repo`) | `projects[].repo` | where PRs are opened |
| Local checkout of that repo | `projects[].repoPath` | absolute path |
| Trigger label | `roles.planner.triggers.labels` and `roles.worker.triggers.labels` | conventionally `looper:plan` |
| Feishu/generic webhook env var | `notifications.webhook.urlEnv` | with `enabled = true`, `format = "feishu"`, `levels = ["action_required", "failure"]` |

The config file is read at daemon startup, so start (or restart) `looperd` after editing it.

## Environment variables to export

Both are read from the daemon environment and are never written into the config file:

```bash
# The Plane API key (name matches providers[].tokenEnv; conventionally PLANE_API_KEY).
export PLANE_API_KEY='plane_api_xxx'

# The Feishu (or generic) webhook URL (name matches notifications.webhook.urlEnv).
export LOOPER_FEISHU_WEBHOOK_URL='https://open.feishu.cn/open-apis/bot/v2/hook/xxxx'
```

You still need an authenticated `gh` for the GitHub code side (`gh auth login`).

## Example `config.json`

The bootstrap command above produces a config equivalent to the following (defaults elided for brevity):

```json
{
  "providers": [
    {
      "id": "plane-acme-design",
      "kind": "plane",
      "baseUrl": "https://plane.powerformer.net/api/v1",
      "tokenEnv": "PLANE_API_KEY",
      "workspace": "acme-design",
      "projectId": "49832a02-3158-4faf-bf2f-d0e39c40c7e6"
    }
  ],
  "projects": [
    {
      "id": "open-design",
      "name": "open-design",
      "provider": "plane-acme-design",
      "repo": "acme/open-design",
      "repoPath": "/absolute/path/to/open-design"
    }
  ],
  "roles": {
    "planner": {
      "autoDiscovery": true,
      "triggers": { "labels": ["looper:plan"], "labelMode": "all", "requireAssigneeCurrentUser": false }
    },
    "worker": {
      "autoDiscovery": true,
      "triggers": { "labels": ["looper:plan"], "labelMode": "all", "requireAssigneeCurrentUser": false }
    }
  },
  "notifications": {
    "webhook": {
      "enabled": true,
      "urlEnv": "LOOPER_FEISHU_WEBHOOK_URL",
      "format": "feishu",
      "levels": ["action_required", "failure"]
    }
  }
}
```

Validation rules for a plane provider/project:

- `providers[].kind = "plane"` requires `tokenEnv`, `workspace`, and `projectId`. `baseUrl` is optional (defaults to the public Plane API base) but must be an absolute `http(s)` URL when set.
- The bound project requires `provider` (the plane provider id) and `repo` (the GitHub code repo, `owner/name`).

## Interactive HITL (mid-run ask / answer)

The `notifications.webhook` block above is **send-only** (an incoming-webhook URL): it posts run updates but can't collect an answer. To let an agent **pause mid-run, ask a human, and resume the same session with the reply**, enable HITL and switch the webhook to Feishu **app-bot** mode, which sends an interactive card with option buttons and receives the click back.

```jsonc
{
  "hitl": { "enabled": true },
  "notifications": {
    "webhook": {
      "enabled": true,
      "mode": "app",                                // app-bot instead of incoming-webhook
      "appIdEnv": "LOOPER_FEISHU_APP_ID",           // env var NAMES, never the secret values
      "appSecretEnv": "LOOPER_FEISHU_APP_SECRET",
      "chatId": "oc_xxxxxxxxxxxxxxxx",              // target group/chat for ask-cards
      "verificationTokenEnv": "LOOPER_FEISHU_VERIFY_TOKEN"  // required for the inbound callback
    }
  }
}
```

How it works:

1. When an agent hits a genuine blocker it writes `.looper/ask.json` (`{"question": "...", "options": ["A", "B"]}`) and stops. The worker suspends the loop to `awaiting_human` and sends the ask-card to `chatId`.
2. A human answers one of two ways:
   - **Click a button** on the card → Feishu POSTs the card action to `POST <daemon>/api/v1/hitl/feishu` (point the app's *card callback / event* URL there).
   - **API** → `POST <daemon>/api/v1/loops/<seq>/respond -d '{"answer":"A"}'` (no Feishu needed).
3. The loop transitions back to `running` and the agent **resumes the same vendor session** (Codex/Claude/OpenCode) with the decision injected — not a fresh run.

**Security — the inbound callback is fail-closed.** `/hitl/feishu` delivers human text straight into a coding-agent session, so it verifies the Feishu app **Verification Token** on every request:

- `verificationTokenEnv` **must** be set (to the env var holding your app's Verification Token) or the callback returns `403` — button clicks won't work until it's configured. This check is independent of `server.authMode` (Feishu's servers can't send a Looper bearer token). The `POST /loops/<seq>/respond` API is unaffected and still governed by `authMode`.
- Turn the app's **Encrypt Key OFF** for the event subscription. Looper reads the callback as plain JSON; an encrypted (`{"encrypt":"…"}`) payload can't be decoded and the answer is dropped.
- Asks with **no options** render a card with no buttons; answer those via the `/respond` API (free-text card replies are not consumed yet).

## How discovery maps Plane labels to roles

- Planner and Worker discovery poll the Plane project and keep only work-items that carry **all** of `roles.<role>.triggers.labels` (label UUIDs are resolved to names first). Configure both roles with the same single trigger label (conventionally `looper:plan`).
- Because Plane assignees are UUIDs — not GitHub logins — discovery keys on the label only. The bootstrap sets `requireAssigneeCurrentUser: false`; do not set it to `true` for plane projects or discovery will never match.
- Coordinator and Fixer discovery lanes are skipped for plane projects (they are GitHub-only). Reviewer runs against the GitHub PRs that Worker opens.
- Label and comment mutations Looper performs during a run go to Plane: adding a label creates it if missing and merges (never clobbers) the work-item's existing labels; comments are posted as `comment_html`.

## Known follow-ups

These are documented gaps, not blockers:

- **Discovery cache.** Each discovery tick re-fetches all labels and all work-items for both Planner and Worker, so large projects make the discovery lane slow. A short-lived per-tick cache is a planned optimization.
- **State-group filtering.** The provider currently reports every returned work-item as `open` and does not drop Plane `completed`/`cancelled` items. A work-item that reaches a done state but still carries the trigger label would be re-discovered until the label is removed. Planned: resolve Plane state groups and filter out done/cancelled items (fail-open so active items are never dropped).
- **Comment updates.** Plane comment ids are UUIDs, which do not fit Looper's integer comment id, so Worker posts a fresh progress comment on each status transition instead of editing one in place.

## Verify

1. Export `PLANE_API_KEY` (and the Feishu webhook env if used) in the daemon's own shell, then start it: `looperd`.
2. Confirm the config loaded and the plane project is registered:
   ```bash
   curl -sS "http://127.0.0.1:17310/api/v1/status"
   ```
3. Confirm it claims a labelled work-item. The CLI has no loop listing — watch the daemon's own output, the dashboard, or `GET /api/v1/loops`. A work-item carrying the trigger label (e.g. `looper:plan`) should produce a planner loop within one poll interval, and Worker opens the PR on the GitHub `repo`.
