# Plane task-source + Feishu HITL reference for agents

Use this when the user's issues live in a [Plane](https://plane.so) project instead of GitHub, but the code and pull requests stay on GitHub. Looper reads work-items from Plane (the task source) and opens/reviews PRs on the GitHub `repo` (the code forge). For end-user prose, see [`docs/plane-provider.md`](../../../docs/plane-provider.md).

## When to use

- Issues/tasks are tracked in Plane, and the team does not want a redundant GitHub issue mirror.
- Code, PRs, and reviews remain on GitHub (so an authenticated `gh` is still required).

If issues already live on GitHub, use the default GitHub project setup instead.

## Write the config by hand

There is no generator for this shape. The `looper bootstrap --provider plane` flow, and every flag it took, was removed with the old CLI. Run `looper init` for a starter `~/.looper/config.toml`, then add the sections below by hand. A plane project cannot be created via the project API alone: a Plane binding lives in the config file's `providers` + `projects` sections, which the project API refuses to modify.

| Value you need | Where it goes | How to get it |
| --- | --- | --- |
| Plane workspace slug | `providers[].workspace` | Plane URL or `plane api me` |
| Plane project UUID | `providers[].projectId` | `plane api project list` |
| Plane REST base URL | `providers[].baseUrl` | your Plane deployment, e.g. `https://plane.example.com/api/v1` |
| Env var holding the Plane API key | `providers[].tokenEnv` | conventionally `PLANE_API_KEY` |
| GitHub code repo (`owner/repo`) | `projects[].repo` | the repo where PRs are opened |
| Local checkout of that repo | `projects[].repoPath` | absolute path |
| Trigger label | `roles.planner.triggers.labels` | conventionally `looper:plan` |
| Env var holding the Feishu webhook URL | `notifications.webhook.urlEnv` | conventionally `LOOPER_FEISHU_WEBHOOK_URL` |

## Two env vars to export

These are read from the daemon environment and never written to the config. Export them before starting `looperd`:

```bash
export PLANE_API_KEY="<plane-api-key>"                      # matches providers[].tokenEnv
export LOOPER_FEISHU_WEBHOOK_URL="<feishu-bot-webhook-url>" # matches notifications.webhook.urlEnv
```

Redact both as `***` in any summary. An authenticated `gh` is still needed for the GitHub code side.

## Config shape to write

```json
{
  "providers": [
    { "id": "plane-<workspace>", "kind": "plane",
      "baseUrl": "https://plane.powerformer.net/api/v1",
      "tokenEnv": "PLANE_API_KEY",
      "workspace": "<workspace-slug>",
      "projectId": "<plane-project-uuid>" }
  ],
  "projects": [
    { "id": "<repo-name>", "name": "<repo-name>",
      "provider": "plane-<workspace>",
      "repo": "<owner>/<repo>",
      "repoPath": "<project-path>" }
  ],
  "roles": {
    "planner": { "autoDiscovery": true, "triggers": { "labels": ["looper:plan"], "labelMode": "all", "requireAssigneeCurrentUser": false } },
    "worker":  { "autoDiscovery": true, "triggers": { "labels": ["looper:plan"], "labelMode": "all", "requireAssigneeCurrentUser": false, "planeAssigneeId": "<your-plane-member-uuid>" } }
  },
  "notifications": { "webhook": { "enabled": true, "urlEnv": "LOOPER_FEISHU_WEBHOOK_URL", "format": "feishu", "levels": ["action_required", "failure"] } }
}
```

Key facts an agent must not get wrong:

- Plane assignees are UUIDs, not GitHub logins, so `requireAssigneeCurrentUser` (which resolves the GitHub login) can't route them — keep it `false`, or discovery matches nothing.
- To route Plane work-items **per person** (so each teammate's looper only picks up its owner's items instead of every looper racing for every labelled item), set `triggers.planeAssigneeId` to that person's Plane member UUID. Empty = label-only discovery (fine when a single central looper consumes the project). Ignored for github/forgejo providers. Get the UUID with the `plane` CLI: `plane api me` → the `id:` line (or `plane api member workspace-list` for anyone's; raw: `curl -H "X-API-Key: <key>" .../api/v1/users/me/` → `id`). Get the `projectId` with `plane api project list`.
- `repo` is the GitHub code repo where PRs land; `workspace`/`projectId` on the provider point at Plane.
- Coordinator and Fixer lanes are skipped for plane projects; Reviewer runs against the GitHub PRs Worker opens.

## Verify (confirm Plane discovery)

After exporting the env vars and starting the daemon:

```bash
looperd                  # in its own shell, with both env vars exported
looper status            # daemon reachable; the plane project should be listed
```

A labelled Plane work-item should yield a planner/worker loop within one poll interval. Watch for it in the daemon's own output, on the dashboard, or at `GET /api/v1/loops` — the CLI has no loop listing.

A Plane work-item that carries the trigger label (e.g. `looper:plan`) should produce a planner loop, and Worker opens the PR on the GitHub `repo`. If nothing is discovered, confirm: `PLANE_API_KEY` is exported and valid, the work-item carries the exact trigger label, and `requireAssigneeCurrentUser` is `false`.

## Known follow-ups

- Discovery re-fetches all Plane labels + work-items each tick (slow on large projects); a per-tick cache is planned.
- The provider reports all work-items as `open` and does not yet drop Plane `completed`/`cancelled` items — a done item that keeps the trigger label is re-discovered until the label is removed.
- Worker posts a fresh progress comment per status transition (Plane comment ids are UUIDs, so in-place edits are not supported).
