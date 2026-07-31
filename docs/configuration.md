# Configuration guide

This document explains Looper's canonical config taxonomy, default config location, supported file formats, project override rules, and the legacy-to-canonical migration story.

## Install layout notes

For the default supported install flow:

- `looper` and `looperd` are installed as separate GitHub Release Go binaries (or built from source)
- you write the config file and start `looperd` yourself — there is no managed daemon install or `looper daemon *` lifecycle
- `looperd` holds a database-specific advisory lock beside each SQLite file. It holds a shared lock while serving a compatible schema and upgrades to an exclusive lock only while applying startup migrations; this prevents a migration from racing a serving daemon. A daemon configured to auto-migrate fails fast if another compatible holder is active.

Keep the runtime directory (`~/.looper` by default, or the directory containing `storage.dbPath`) on a local filesystem. The database lock uses OS file locking and is not designed for NFS-style shared filesystems. If shutdown retains SQLite after an undrained ownership failure, it retains the shared lock too; start a replacement only after the owning process has exited. Tunnel-mode webhook secrets live under the same runtime directory in `secrets/` and must be mode `0600`.

## Network mode summary

Looper has two project-level network modes:

- `projects[].network.mode = "off"` — local-only operation. `looper:target:*` labels are ignored and the classic single-Node assignee/review-request behavior stays unchanged.
- `projects[].network.mode = "routed"` — multi-Node operation coordinated through `loopernet`.

Authority stays split on purpose:

- GitHub work intent stays on GitHub: `looper:worker-ready` for Worker and GitHub review requests for Reviewer.
- exactly one `looper:target:<node_name>` label is the exact-Node authority in Routed mode.
- the `loopernet` lease is a mutation fence for Coordinator only; it does not become the source of truth for work intent.

Operational notes:

- `loopernet` centralizes webhook ingress and Node wakeups, but it must not mutate GitHub on its own.
- Coordinator writes coarse GitHub authority first, then writes the exact target label last.
- polling remains enabled as fallback and drift recovery when webhook delivery or SSE wakeups are missed.
- when enabling Routed mode / network membership in config, Looper rejects enrollment when Planner or Fixer auto-discovery is still enabled for those projects; disable those settings first or opt projects into Routed mode manually.

The formal contract is documented in ADRs [0007](adr/0007-coordinator-admission-assignment-authority.md) through [0011](adr/0011-coordinator-control-plane-for-routed-projects-v1.md).

For runtime deployment details — container image, required environment variables, persistence, and the current single-instance recommendation — see [loopernet deployment](loopernet-deployment.md).

## Webhook delivery modes

`webhook.enabled=true` supports two delivery modes:

- `gh-forward` (default): Looper starts `gh webhook forward` against each configured repo and receives deliveries on the daemon API route `/webhook/forward`.
- `tunnel`: Looper creates an ordinary GitHub repository webhook per repo and expects the user to run a tunnel to `127.0.0.1:<webhook.listenPort>`.

Tunnel-mode example:

```toml
[webhook]
enabled = true
mode = "tunnel"
listenPort = 8765
publicBaseUrl = "https://looper.example.com"
fallbackPollIntervalSeconds = 300

[[projects]]
id = "looper"
name = "looper"
repoPath = "/Users/me/src/looper"

[[projects]]
id = "private"
name = "private"
repoPath = "/Users/me/src/private"
[projects.webhook]
mode = "gh-forward"
```

Rules:

- `webhook.mode` is the global default. A project may override with `projects[].webhook.mode`.
- `tunnel` requires `webhook.listenPort` between `1024` and `65535` and an HTTPS `webhook.publicBaseUrl`.
- The tunnel URL for repo `owner/repo` is `{publicBaseUrl}/webhook/owner/repo`.
- Looper binds only `127.0.0.1:<listenPort>`; it does not run or supervise `cloudflared`, `ngrok`, Tailscale Funnel, or any reverse proxy.
- Looper stores the remote GitHub hook id in SQLite and the HMAC secret in `secrets/webhook_<owner>_<repo>.key` with mode `0600`.
- Removing a project or switching it away from `tunnel` marks the local hook record orphaned; it does not delete the GitHub hook automatically.

## How config loading works

`looperd` loads configuration in this order:

1. built-in defaults
2. config file
3. environment variables
4. CLI flags

Later layers override earlier ones. Objects are merged deeply, arrays are replaced as a whole, and omitted fields keep the previous-layer value.

### Dynamic reload

`looperd` watches the selected config file and publishes a candidate atomically only when every changed effective field is hot-safe. A claim made after publication uses the new snapshot; an already active run keeps the snapshot it started with. Invalid candidates and candidates containing restart-bound changes leave the last-known-good snapshot active and expose diagnostics at `/dashboard/config`. Mixed candidates are rejected as a whole rather than partially applied.

The hot-safe surface is an explicit allowlist (see [ADR-0014](adr/0014-config-file-is-global-runtime-policy-authority.md) for field classification):

- `agent.vendor` (including adding the first vendor after daemon startup), `agent.model`, individual `agent.env` entries, and the canonical idle/max-runtime fields under `agent.timeouts.*`
- named `agent.profiles.<id>` entries and their `vendor` / `model` leaves (whole-map `agent.profiles` is not a dashboard path; profile ids match `[A-Za-z0-9_-]+`)
- coding-role agent bindings: `roles.{planner,worker,reviewer,fixer}.agent.{profile,vendor,model}`
- `scheduler.maxConcurrentRuns` and `scheduler.slowLaneWarnThresholdMs`
- `notifications.inApp` and the current `notifications.osascript.*` fields; notification webhooks and Feishu notification transport are restart-bound
- the current `disclosure.*` fields
- `defaults.allowAutoCommit`, `defaults.allowAutoPush`, `defaults.allowRiskyFixes`, `defaults.openPrStrategy`, and `defaults.addSnapshotMode`; `defaults.baseBranch` is restart-bound because configured project records materialize it
- `instructions.enabled` only
- the current Planner discovery/trigger/instruction fields; all current Worker and Fixer discovery/trigger/instruction fields; Reviewer discovery, most behavior, and instructions; and Coordinator polling, triage, dispatch, and merge-watch policy except `mergeWatch.transientRetries`
- `tools.looperPath` and `tools.osascriptPath`

Profile and role agent vendor/model fields are hot-safe curated identity fields: a claim made after publication resolves against the new config; an already active run keeps the frozen agent snapshot it started with (resume/retry lineages copy that predecessor snapshot rather than re-resolving live config).

`agent.vendor` can switch from one configured vendor to another when `agent.params` is empty and no explicit model is being silently carried across vendors. If `agent.model` is set, change or unset it in the same candidate; an unchanged explicit model blocks that vendor-to-vendor switch. Clearing a configured vendor uses the same guard, so a retained profile cannot be laundered through an intermediate `null`. The same leave/switch guards apply to each coding role's *resolved* vendor after global → profile → role overlay. Configuring the first vendor may use an already prepared model/params profile. Continuations of failed or interrupted runs copy the predecessor's durable `agent_snapshot_json` (sticky identity across the retry lineage) while retaining checkpoint, worktree, HITL answer, and queued human instructions. Only legacy predecessors with a null snapshot adopt the runner's current resolved identity. Looper never sends an old vendor's native session ID to a different CLI.

Notably, `agent.nativeResume`, `agent.params`, `roles.coordinator.enabled`, `roles.gatekeeper.*`, `instructions.maxBytes`, all `hitl.*`, all `intake.*`, all `notifications.webhook.*`, `roles.reviewer.behavior.loop.quietPeriodSeconds`, `roles.reviewer.behavior.loop.minPublishIntervalSeconds`, `roles.reviewer.behavior.retry.maxDelayMs`, `roles.coordinator.mergeWatch.transientRetries`, and `roles.coordinator.dependencies.*` require restart. `agent.params` stay global, file-only, and restart-bound; the dashboard does not edit params. The scheduler retry budget/base delay and these Reviewer timing fields are durable queue-scheduling inputs; Coordinator transient retries are persisted as a remaining budget, so they are also restart-bound. Listener, storage, daemon, logging, webhook/network topology, providers/projects, scheduler polling/cache, and `tools.gitPath`/`tools.ghPath` also require restart. New fields are restart-bound until explicitly classified.

Deprecated file-layer aliases for `agent.timeouts.{planner,worker,reviewer,fixer}Seconds`, `defaults.allowAutoApprove`, and `defaults.fixAllPullRequests` are normalized into their canonical hot-safe fields so existing files can still reload without a restart. They remain file-only compatibility syntax: the dashboard exposes and writes only canonical paths, and a canonical dashboard edit removes the corresponding alias leaf so a later unset cannot resurrect the old value.

The dashboard is a curated field-level editor, not a raw file editor. Environment- and CLI-owned fields are read-only. `agent.env` values are write-only (only key names are returned), while `server.localToken` (or `LOOPER_TOKEN`), `daemon.environment`, and `agent.params` remain outside dashboard editing. Projects remain under the Projects API and SQLite authority. When token authentication is not configured, `server.host` must be `localhost` or a literal loopback IP; wildcard, LAN, public, reverse-proxy, and custom-hostname binds require `local-token`. This startup rule avoids treating a loopback reverse proxy as proof that the original caller was local. As defense in depth, `PATCH /api/v1/config` still accepts only direct requests whose peer and Host authority are loopback and rejects proxy-forwarding headers; in `local-token` mode it requires the normal token authentication.

Every dashboard read includes the revision of the exact file generation that produced its published values, and every patch must submit that revision. The revision check and a final identity/mode/byte check catch changes present before that final check, including a newer generation not yet accepted by the reload loop. The writer then uses a crash-safe atomic rename. Portable filesystems do not offer a conditional compare-and-rename, so an external editor racing in the tiny interval between the final check and rename can still be replaced; avoid simultaneous manual and dashboard writes. A successful patch preserves the selected TOML/YAML/JSON format, unknown top-level extension sections and their native scalar values, and ordinary permission bits, but serialization can canonicalize comments, quoting, key/table order, and other lexical formatting; ACLs and extended filesystem metadata are not guaranteed to survive atomic replacement. Dashboard patching refuses a symlinked config path; edit the symlink target directly instead.

## Supported formats and default path

Looper accepts config files in these formats:

- `.toml`
- `.yaml`
- `.yml`
- `.json`

Canonical default path:

- `~/.looper/config.toml`

Config source selection precedence is:

1. `--config`
2. `LOOPER_CONFIG`
3. default-path discovery

`~/.looper` itself is resolved as `LOOPER_HOME` → `HOME` → the OS user home directory. Setting `LOOPER_HOME` relocates every default-derived path together — config discovery, `storage.dbPath`, `daemon.logDir`, `storage.backupDir`, and the worktree roots used when a project record carries no `worktreeRoot` metadata. Use it to run a second instance, or to keep a test binary out of your real state directory; explicit config values still win over the defaults it produces.

Default-path discovery checks, in order:

1. `~/.looper/config.toml`
2. `~/.looper/config.yaml`
3. `~/.looper/config.yml`
4. `~/.looper/config.json`

Behavior:

- if exactly one supported default config file exists, Looper loads it
- if both `~/.looper/config.toml` and legacy `~/.looper/config.json` exist, Looper prefers `config.toml`
- any other multiple-default-file combination fails clearly instead of guessing
- if none exist, Looper continues with built-in defaults and treats `~/.looper/config.toml` as the canonical path for newly generated config

To migrate the legacy default JSON config explicitly, run:

```bash
# looper config migrate  # removed — edit the config file by hand
```

Useful migration flags:

- `--from <path>` to read a non-default source config file
- `--to <path>` to write somewhere other than the default canonical TOML path
- `--dry-run` to preview the canonical output without touching user files
- `--force` to overwrite an existing destination after creating a backup

Custom config path examples:

- `LOOPER_CONFIG=/absolute/or/relative/path/to/config.toml`
- `looperd --config /absolute/or/relative/path/to/config.toml`

Relative config paths are resolved from the current working directory used to start `looperd`.

## Canonical taxonomy

Looper's frozen canonical top-level config roots are:

| Root | Purpose |
| --- | --- |
| `server` | network-facing API/server configuration |
| `daemon` | daemon lifecycle, runtime paths, and local process behavior |
| `storage` | sqlite/database/backups/history retention and storage-specific settings |
| `scheduler` | loop scheduling, concurrency, polling, and timing policy that is not role-specific |
| `agent` | model/provider/executor defaults that apply across roles unless overridden more locally |
| `logging` | logs, verbosity, sinks, and diagnostic controls |
| `notifications` | user notifications such as osascript or future notifier integrations |
| `disclosure` | disclosure/stamping policy for outward-facing automation output |
| `tools` | external tool paths and tool-specific execution settings such as `git`, `gh`, and `osascript` |
| `package` | packaging, upgrade, and distribution policy |
| `defaults` | user-facing default policy that does not belong to a narrower domain |
| `instructions` | global instruction-system settings that are not role-specific instruction content |
| `roles` | role-specific config grouped by role name, for example `roles.<role>` |
| `providers` | Optional GitHub provider definitions used as repository-identity discriminators |
| `projects` | per-project metadata and supported project-scoped overrides |

### Project authority and import

`[[projects]]` is a declarative startup import, not a second runtime project store. During daemon startup Looper validates and transactionally imports configured projects into SQLite, then builds the runtime Project Catalog exclusively from active database records. Scheduler, Webhook, Network, and Roles all capture that same Catalog.

- Removing a config-managed project from `[[projects]]` archives its SQLite record on the next startup.
- Config import never removes API-managed projects.
- Reusing an active API-managed project ID in `[[projects]]` fails startup instead of transferring ownership implicitly. An archived API record is the explicit handoff authority: config import may claim that ID and replace it with an active config-managed record after retiring the old worktree registrations; it does not alter physical checkouts.
- CLI/API add and remove operations publish one atomic Catalog replacement after the database commit; already-started work keeps its captured snapshot, while new work observes the new Catalog.
- A project referencing a missing Provider fails validation; it never falls back to GitHub.

See [ADR-0012](adr/0012-sqlite-project-authority.md) for the Authority and lifecycle decision.

To move an existing API-managed project into `[[projects]]`, first remove its new config entry so the daemon can start, then verify the exact target project ID and send `DELETE /api/v1/projects/<id>` while the daemon is running. Stop the daemon, restore the complete `[[projects]]` entry (including any `provider` and `repo`), and restart. The DELETE archives the API record, terminates its old loops, and retires its worktree registrations without touching their physical checkouts; that archived bit is the durable, explicit ownership handoff that permits config import to claim the same ID. Do not delete SQLite rows directly.

Legacy top-level `reviewer.*` input is compatibility-only. The canonical reviewer behavior home is `roles.reviewer.behavior.*`.

Schema migration is independent from config-file format migration: precedence stays `defaults → config file → environment variables → CLI flags` regardless of whether a file still uses legacy reviewer paths or legacy JSON defaults.

There is no `looper config migrate` on the stripped CLI. Edit the config file by hand (or use the dashboard for curated hot-safe fields). Normal daemon startup never rewrites config files implicitly.

## Minimal setup

In the simplest setup, you can rely on defaults and only create a config file when you need to customize behavior.

`agent.vendor` does not have a built-in default. It is the inheritance base and the zero-diff default for every coding role, but it is not mandatory when a role resolves vendor from `agent.profiles` or `roles.<role>.agent` alone. Set a global vendor when you want one shared identity, or when coordinator triage LLM should run (triage uses the global agent only).

Example minimal `~/.looper/config.toml`:

```toml
[agent]
vendor = "opencode"

[[projects]]
id = "looper"
name = "Looper"
repoPath = "/absolute/path/to/repo"
```

Existing global-only configs remain zero-diff: a single `agent.vendor` / `agent.model` still applies to every coding role (planner, worker, reviewer, fixer) until you add profiles or per-role bindings.

Configured vendors have an explicit [coding-agent runtime contract](agent-runtime-contract.md)
covering continuation, event, supervision, and security behavior. A vendor name
does not imply support for every capability; unsupported policy gates fail
closed or use the documented fresh-session fallback.

## Multi-role agent vendor and model

Coding roles can share one global agent or override vendor/model per role. Overrides are identity-only (vendor + model). Shared executor settings such as `agent.params`, `agent.env`, timeouts, and `agent.nativeResume` stay global.

### Named profiles (`agent.profiles`)

Define reusable vendor/model pairs under `agent.profiles.<id>`. Each profile may set `vendor`, `model`, or both (at least one is required). Profile ids are non-empty, trimmed, and match `[A-Za-z0-9_-]+`.

Profiles do not carry params, env, or timeouts.

### Per-role bindings (`roles.<role>.agent`)

Optional on the four coding roles only: `planner`, `worker`, `reviewer`, and `fixer`.

This named form remains compatibility input. For new canonical registry configuration, put the same binding at `roles.coding.<role>.agent`; when both forms set a field, `roles.coding.<role>` wins.

| Field | Purpose |
| --- | --- |
| `profile` | Name of an entry in `agent.profiles` |
| `vendor` | Inline vendor override |
| `model` | Inline model override |

A role may use a profile ref, inline vendor/model, or both (inline wins over the selected profile for the same field).

Project-level `projects[].roles.*.agent` bindings are **not supported**. Agent identity is global-only; project role partials that set agent fields fail validation.

### Resolve order

For each coding role, Looper overlays identity in this order:

1. **Global** `agent.vendor` / `agent.model`
2. **Role profile** — from the effective canonical registry (projected from `roles.<role>.agent` and then overlaid by `roles.coding.<role>.agent`)
3. **Role inline** — the registry's inline vendor/model fields win over the selected profile

A role is runnable only when the overlay leaves a non-empty vendor. Missing global vendor is fine when a profile or role inline supplies one.

### Model semantics

| Config value | Meaning |
| --- | --- |
| field omitted / unset | inherit from the previous layer (or remain unset) |
| non-empty string | explicit model for that layer |
| empty string `""` | suppress inherited model → vendor default |

After the full overlay, an empty-string model is kept as an explicit empty binding (not the same as unset): the vendor CLI uses its own default, and any global `agent.params` `--model`/`-m` flags are stripped so they cannot override the suppression.

### Coordinator triage

Coordinator triage LLM uses the **global** agent only (`agent.vendor` / `agent.model`, plus global params/env/timeouts). It does not read `roles.coordinator.agent` or coding-role profile bindings. If global `agent.vendor` is unset, triage LLM is skipped; coding roles that resolve via profile or role bindings can still run.

### Hot reload and frozen runs

- Legacy profile and named-role agent paths are hot-safe for **new claims** after a successful config publication. A `roles.coding.*` overlay is restart-bound with the rest of the canonical registry.
- In-flight runs keep the immutable config snapshot (and durable per-run agent snapshot) they started with; resume/retry copies the predecessor run's agent snapshot rather than re-resolving live config.
- `agent.params` remain global, file-only, and restart-bound. The dashboard does not edit params.
- Every `daemon.worktreeCleanup.*` leaf is hot-safe. The cleanup loop always runs and rereads `enabled` and `interval` on each wake, and a successful publication wakes it immediately, so enabling cleanup or shortening its interval takes effect without waiting out the previous schedule or restarting the daemon. The `daemon.worktreeCleanup` object itself stays restart-bound.

### Example: different reviewer vs worker models

TOML:

```toml
[agent]
vendor = "codex"
model = "gpt-5"

# Shared identity presets (vendor + model only).
[agent.profiles.fast]
vendor = "codex"
model = "gpt-5-mini"

[agent.profiles.strong]
vendor = "claude-code"
model = "claude-sonnet"

# Worker keeps the global codex/gpt-5 binding (no roles.worker.agent block).

[roles.reviewer.agent]
profile = "strong"
# Optional inline pin on top of the profile:
# model = "claude-opus"

[roles.fixer.agent]
profile = "fast"

# Suppress model so the vendor CLI default is used:
# [roles.planner.agent]
# model = ""
```

Equivalent JSON:

```json
{
  "agent": {
    "vendor": "codex",
    "model": "gpt-5",
    "profiles": {
      "fast": { "vendor": "codex", "model": "gpt-5-mini" },
      "strong": { "vendor": "claude-code", "model": "claude-sonnet" }
    }
  },
  "roles": {
    "reviewer": {
      "agent": { "profile": "strong" }
    },
    "fixer": {
      "agent": { "profile": "fast" }
    }
  }
}
```

With that file, worker and planner resolve to global `codex` / `gpt-5`, reviewer to `claude-code` / `claude-sonnet` via `strong`, and fixer to `codex` / `gpt-5-mini` via `fast`.

## Grok Build (xAI)

Use `grok-build` as the `agent.vendor` identifier. Looper invokes the xAI Grok Build executable as `grok`:

```toml
[agent]
vendor = "grok-build"
```

Authenticate the daemon safely with `grok login --device-auth`, or make `XAI_API_KEY` available in the daemon environment. Do not put API-key values in committed config files or examples.

For fresh unattended runs, Looper supplies `--always-approve` and `--sandbox off` so Grok can update Git metadata outside a linked worktree. Configured agent arguments override these defaults; in particular, operators can select a stricter `--sandbox` when the repository layout permits it, `--permission-mode` may prompt or fail unattended runs, non-`plain` `--output-format` can prevent direct `__LOOPER_RESULT__=` completion-marker parsing, and configured `-p` or `--single` replaces Looper's generated task prompt.

Grok Build support is fresh-run only. Daemon native resume and interactive takeover through `looper resume` are unsupported. A retry uses a fresh checkpoint prompt, and Looper never uses Grok Build's ambient `--continue`.

## Devin CLI (experimental fresh-run)

Use `devin-experimental` as the `agent.vendor` identifier. The persisted name
makes its weaker lifecycle contract machine-visible. Looper invokes the locally
authenticated `devin` executable in non-interactive print mode:

```toml
[agent]
vendor = "devin-experimental"
model = "glm-5-2"
```

Authenticate and inspect the models currently available to the daemon account
before starting Looper:

```bash
devin auth login
devin auth status
devin models list --format json
```

Model availability and pricing are time-sensitive. Pin the exact model UID
reported by `devin models list`; do not rely on a family alias when cost matters.
As of 2026-07-30, the tested account reports `glm-5-2`, `swe-1-7`, and
`swe-1-7-medium` as free. For example, `swe` currently resolves to the paid
`swe-1-7-lightning` variant rather than either free SWE-1.7 variant. Treat this
as time-sensitive evidence, not a permanent pricing guarantee.

Fresh unattended runs default to `--permission-mode dangerous`,
`--respect-workspace-trust false`, and `--print`. Operators can override the
permission and workspace-trust flags through `agent.params.args`. Dangerous
mode lets the agent execute without prompts and is not a filesystem or network
security boundary. Validation-gated runs that request Looper's strict
tool-network restriction therefore fail closed for Devin, as they do for every
non-Codex vendor.

This first integration is fresh-run only. Although Devin CLI exposes resume and
ACP, Looper does not yet capture Devin's ATIF session identity or implement a
shared ACP transport. Daemon native resume and interactive takeover remain
disabled; retries continue from Looper's durable checkpoint in the same
worktree with a fresh Devin session.

## Provider support

GitHub is the only supported provider kind, and github.com is the only host Looper drives end to end.

- `github` — backed by `gh`. Projects without `provider` keep the legacy GitHub autodetection/metadata path.

A `providers` entry is optional and does very little today. Read the field notes before adding one.

```toml
[agent]
vendor = "opencode"

[[providers]]
id = "acme"
kind = "github"

[[projects]]
id = "example"
name = "Example"
repoPath = "/absolute/path/to/example"
provider = "acme"
repo = "acme/example"
```

Provider rules:

- `providers[].id` must be unique.
- `providers[].kind` must be `github`. A configured `forgejo` or `plane` kind is rejected with an explicit unsupported-provider error — both were removed and are never reinterpreted as a supported provider.
- **Upgrading from a config that used a removed provider:** delete the `[[providers]]` entry and any `[[projects]]` bound to it before starting the new daemon. An API-registered project whose stored provider binding is no longer config-owned is automatically archived at startup, rather than being rebound to github.com; its history and original binding stay in SQLite, but it is absent from the runtime catalog. To restore automation, add an explicit `[[projects]]` entry only after the API project is archived, using the intended GitHub identity. If two legacy API projects used the same `owner/name` on different providers, archive or hand off each deliberately; do not delete SQLite rows.
- `providers[].baseUrl` is optional and, when set, must be a `github.com` URL (`github.com`, `www.github.com`, or `api.github.com`). It does not point Looper at a host — the `gh` gateway resolves its own — so a non-github.com value would configure a target Looper cannot drive and is rejected at startup rather than failing later at publish time.
- `providers[].tokenEnv` names an environment variable, **not the credential the GitHub gateway uses.** Planner, worker, reviewer, fixer, webhook, and discovery calls all authenticate through ambient `gh` auth (`gh auth login`). The named variable is copied unchanged from the daemon environment into trusted `looper review submit` child processes and nowhere else.
- A project bound to a provider requires both `provider` and a repo (`owner/name`); a binding without a repo is rejected. The project HTTP API can register a local `repoPath` against a running daemon, but provider bindings themselves are file-managed. Already-started work retains its previous catalog snapshot.
- Config validation rejects two projects whose repository *identities* collide, matched case-insensitively. Every provider resolves to github.com, so the same `owner/name` slug is a duplicate no matter how many provider ids it is split across.

### GitHub Enterprise Server

GHES is **not supported, and configuring it is now rejected at startup** rather than accepted and failed later:

```
providers[0].baseUrl: must be a github.com URL or omitted; GitHub Enterprise Server is not supported
```

The reason it cannot simply be pointed at a host: `providers[].baseUrl` reaches repository identity and nothing else, and the two mechanisms that could route are inconsistent. The gateway derives `gh --hostname` from a three-segment `projects[].repo` (`host/owner/name`), but the review-submit REST branch — which `looper review submit` always takes, because it supplies `commit_id` — interpolates the repo verbatim into `repos/{repo}/pulls/{n}/reviews`, and webhook tunnel routing accepts only `/webhook/{owner}/{repo}`. Both fail after startup, at publish and delivery time.

Threading one host authority through the gateway, review-submit, and the tunnel router is the work that would make GHES real. Until then, failing at startup is the honest outcome.


GitHub live sandbox tests now prefer `LOOPER_E2E_GITHUB_SANDBOX_REPO`. The older `LOOPER_E2E_SANDBOX_REPO` name remains a compatibility alias, but setting both names to different repos fails fast.

## Role model guidance

All role-specific config lives under `roles.<role>`.

- shared role instructions live at `roles.<role>.instructions`
- discovery policy lives at `roles.<role>.discovery.*`
- runtime behavior lives at `roles.<role>.behavior.*` when that split is useful for the role
- coding-role agent identity overlays live at `roles.{planner,worker,reviewer,fixer}.agent` (profile ref and/or inline vendor/model); see [Multi-role agent vendor and model](#multi-role-agent-vendor-and-model)

### Canonical coding-role registry (`roles.coding.<shipped-role>`)

`roles.coding` is the canonical runtime registry for the four compiled agent runners: `planner`, `worker`, `reviewer`, and `fixer`. Its `priority`, `discovery`, `instructions`, and `agent` fields drive the scheduler, runner policy, prompt instructions, and agent identity.

The existing named `roles.<name>.*` sections remain supported as compatibility input: Looper projects them into the registry first, then overlays the matching `roles.coding.<name>` fields. This means a legacy-only configuration keeps its behavior, while an authored registry entry wins for the fields it sets. Reviewer-only behavior such as `behavior` and `specReview` remains under `roles.reviewer.*` because only the reviewer runner implements it.

- Only `planner`, `worker`, `reviewer`, and `fixer` are valid keys. Any other name is rejected because no compiled runner could execute it.
- `gatekeeper` is a compiled policy role, not an authorable registry entry. Its source, priority, and policy cannot be overridden.
- `coordinator` and `triager` are internal lanes, not coding roles.
- `roles.coding.*` is global-only; `projects[].roles.coding` is rejected. Existing project-level named role overrides remain compatible.
- A runner's source is fixed: Planner and Worker use `issue`; Reviewer and Fixer use `pull_request`. If `discovery.source` is written, it must match that source. Source-inapplicable fields are rejected even when set to `false` or an empty string.
- `priority` must be a positive integer. Lower values run earlier. `labelMode` defaults to `all` when omitted.

`roles.coding.*` overlays are restart-bound because the canonical registry is derived runtime state rather than a serialized dashboard field.

```toml
# Worker policy, prompt guidance, identity, and lane order all come from this
# one canonical entry.
[roles.coding.worker]
priority = 25
instructions = "Implement only tasks carrying the ready label."

[roles.coding.worker.discovery]
source = "issue"
enabled = true
labels = ["looper:ready"]
labelMode = "all"
requireAssigneeCurrentUser = false

[roles.coding.worker.agent]
profile = "fast"
```

## Triager admission policy

The internal Triager separates deterministic admission from model classification. `roles.triager.preset` is project-overridable and defaults to `legacy`, which preserves the original model-first seven-condition gate exactly. Opting into a relationship preset makes forge facts—not Issue prose—the admission authority.

| Preset | Owner | Member/collaborator | Past contributor | Unaffiliated |
| --- | --- | --- | --- | --- |
| `personal` | `auto` | `assess` | `assess` | `assess` |
| `maintained-oss` | `auto` | `auto` | `assess` | `assess` |
| `company` | `auto` | `assess` | `assess` | `assess` |
| `contributing` | `assess` | `assess` | `assess` | `assess` |

- `auto` writes a Triage Report and routes directly to Planner without a classification model call.
- `assess` runs classification when `classify = true`, writes the result, and waits for the report-specific `/plan <token>` confirmation. Classification describes the work but cannot authorize routing.
- `ignore` writes the admission decision and retires that source without a model call.
- Bot authors default to `ignore`; `looper:hold` always produces `ignore` for configured presets. Neither can be overridden to `auto`.
- `contributing` can never auto-route, including through a tier override.
- If the forge cannot report repository visibility, a would-be `auto` result degrades to `assess` rather than guessing.

Tier overrides use `owner`, `member`, `past-contributor`, `unaffiliated`, or `bot` keys:

```toml
[roles.triager]
preset = "company"
classify = true

[roles.triager.authorTiers]
member = "auto"
unaffiliated = "ignore"
```

The historic gate remains configurable under `roles.triager.legacy`: `autoRouteConfidence` (`0.8`), `maxAutoRouteRisk` (`low`), `requireInScope`, `requireNoMissingInformation`, `requirePlanner`, and `requireRationale` (all `true`). These fields apply only to `legacy`; an omitted Triager section therefore behaves exactly as before.

## Coordinator config reference

Coordinator is the proactive, stateless issue-intake role. It owns both Triage and Dispatch. Triage writes `triaged` plus the coordinator-owned label namespace. Dispatch consumes `triaged` + `dispatch/*` and derives the actual trigger label from Planner or Worker config instead of redeclaring those labels.

Triage LLM calls use the **global** `agent.vendor` / `agent.model` only (not coding-role profiles or `roles.*.agent` overlays). See [Multi-role agent vendor and model](#multi-role-agent-vendor-and-model).

### Triage settings

Coordinator triage lives under `roles.coordinator.triage.*`:

| Path | Purpose | Default |
| --- | --- | --- |
| `roles.coordinator.enabled` | Turns Coordinator on for the project or globally | `false` |
| `roles.coordinator.pollInterval` | Minimum delay between Coordinator ticks for the same project | `"5m"` |
| `roles.coordinator.triage.triagedLabel` | Durability-commit label written last after comment posting succeeds | `"triaged"` |
| `roles.coordinator.triage.maxIssueAgeDays` | Bootstrap guard for fresh issues only | `7` |
| `roles.coordinator.triage.maxPerTick` | Per-tick cap on issues processed for triage | `5` |
| `roles.coordinator.triage.disposition.outOfScopeLabel` | Label reused for `out-of-scope` | `"wontfix"` |
| `roles.coordinator.triage.disposition.unclearLabel` | Label used for `unclear` | `"needs-info"` |
| `roles.coordinator.triage.disposition.reTriageOnAuthorReply` | Re-opens the triage loop when the original author clarifies a `needs-info` issue | `true` |

Coordinator clears and rewrites its own label namespace on each successful triage pass: `kind/*`, `area/*`, `complexity/*`, `dispatch/*`, `wontfix`, and `needs-info`. It then posts or edits the marker comment and writes `triaged` last.

### Dispatch settings

Coordinator dispatch lives under `roles.coordinator.dispatch.*`:

| Path | Purpose | Default |
| --- | --- | --- |
| `roles.coordinator.dispatch.mode` | Chooses `human-gated` or `autonomous` dispatch | `"human-gated"` |
| `roles.coordinator.dispatch.assignTo` | Optional GitHub assignee added before the trigger label commit | `""` |
| `roles.coordinator.dispatch.humanGate.slashCommands` | Accepted start-of-line slash commands | `[`"/plan"`, `"/implement"`]` |
| `roles.coordinator.dispatch.humanGate.allowedUsers` | Extra users allowed to dispatch even without repo write access | `[]` |
| `roles.coordinator.dispatch.autonomous.delayMinutes` | Grace window after `triaged` before autonomous dispatch can commit | `30` |
| `roles.coordinator.dispatch.autonomous.holdLabel` | Legacy compatibility-only veto label for autonomous dispatch | `"looper:hold"` |

### Post-merge digest

The optional `roles.coordinator.postMergeDigest` lane sends one timezone-aware
daily audit after the configured time and exposes the same four sections in the
dashboard. It is disabled by default. The digest reads only durable local
`event_logs`, pull-request snapshots, and loop records; it does not make a
GitHub read or ask an agent to supply evidence. A successful notification (or a
quiet-day marker when `includeEmpty = false`) is recorded locally so a tick
retry cannot duplicate the day. Each scheduled run reports the previous
completed local calendar day, so activity after the delivery time is not lost.

| Path | Purpose | Default |
| --- | --- | --- |
| `roles.coordinator.postMergeDigest.enabled` | Enable the daily digest scheduler lane | disabled |
| `roles.coordinator.postMergeDigest.schedule` | Local 24-hour `HH:MM` delivery time | required when enabled |
| `roles.coordinator.postMergeDigest.timezone` | IANA timezone used for day boundaries and schedule | required when enabled |
| `roles.coordinator.postMergeDigest.includeEmpty` | Deliver a one-line digest on days with no activity | `false` |
| `roles.coordinator.postMergeDigest.maxItems` | Maximum items retained per section | required, `1..200` |

Sections are `Merged`, `Closed-and-regenerated`, `Awaiting human`, and
`Anomalies`. Gate/reviewer summaries, retry fingerprints, and diff sizes are
included only when corresponding durable records exist; missing evidence is
reported as an anomaly rather than inferred from live GitHub state.

The digest block is global-only; project role overrides reject
`projects[].roles.coordinator.postMergeDigest` because the scheduler has one
process-wide daily delivery and cannot safely run a different schedule per
project.

Behavior notes:

- `/plan` maps to the first planner trigger label at `roles.planner.triggers.labels[0]`
- `/implement` maps to the first worker trigger label at `roles.worker.triggers.labels[0]`
- autonomous mode uses the existing `dispatch/*` label to choose the same derived trigger labels
- Coordinator never stores its own dispatch state; the authority chain stays on GitHub labels, comments, and timeline events
- `roles.coordinator.dispatch.autonomous.holdLabel` is compatibility-only for coordinator autonomous dispatch; the official global hold contract is `looper:hold`

## Coordinator mark-ready

Coordinator can take a looper-authored draft PR out of draft once CI is green, so review starts without a human clicking "Ready for review". It lives under `roles.coordinator.markReady.*` and runs inside the merge-watch lane:

| Path | Purpose | Default |
| --- | --- | --- |
| `roles.coordinator.markReady.enabled` | Publishes eligible looper-authored drafts | `false` |
| `roles.coordinator.markReady.scope` | Which drafts Looper may publish; `looper-only` is the only accepted value | `"looper-only"` |

A draft is published only when every one of these holds. Any one failing leaves the draft exactly as it is, and the next tick looks again:

- `looper-only` scope: the PR carries a `looper:` label **and** links the tracked issue with a closing reference
- the PR itself is authored by the account the daemon runs as — a maintainer's own draft over machine-written commits is theirs to publish
- nobody has converted the PR back to draft: a `convert_to_draft` timeline event performed by anyone but the daemon is an explicit "not ready" and skips the PR for good
- all required checks on the head are green — branch protection names them (matching the required GitHub App where protection binds one), and where it names none, every check observed on the head counts
- at least one check is known. A head with no required checks *and* no observed checks says nothing about CI, so the draft waits rather than publishing seconds before the first workflow registers
- GitHub reports the branch as mergeable and not conflicting
- no `looper:hold` and no `do-not-merge` label
- every commit on the branch is attributed to the account the daemon runs as

Immediately before publishing, the PR is read once more and every guard above that does not depend on the head is re-evaluated, with the head compared against the one the checks and commits were read from. A push, a new hold label, or a human publishing first between the decision and the mutation leaves the draft alone; `gh pr ready` takes no head argument, so making the evidence true again is the only conditional mutation available.

The lane only ever considers open drafts: it lists them once per tick and intersects that with the issue's linked PRs, so a merged or already-published reference costs nothing on later ticks.

Marking ready is idempotent: a human clicking "Ready for review" first is success, not an error.

**Publishing alone does not start a review.** It emits `ready_for_review`, and that delivery does wake the reviewer lane — but the reviewer refuses a PR authored by the account it runs as unless `roles.reviewer.discovery.triggers.enableSelfReview` is true, and GitHub will not let anyone request review from a PR's own author. So with the shipped defaults (`requireReviewRequest = true`, `enableSelfReview = false`) and a single daemon identity, mark-ready produces drafts that publish and then sit. Make review actually happen with one of:

- a **distinct reviewer identity** — a second daemon, or a routed reviewer node, running under its own GitHub login. The PR is not self-authored from its point of view, so the default reviewer configuration applies unchanged.
- `roles.reviewer.discovery.triggers.enableSelfReview = true`, accepting that the same account both writes and reviews, and set `triggers.requireReviewRequest = false` so discovery has a non-request path.

Coordinator logs a warning at startup when `markReady.enabled` is true and the effective reviewer configuration can never claim a looper-authored PR. Mark-ready does not change any reviewer default on your behalf.

## Hold labels

Official hold labels are fixed:

- `looper:hold`
- `looper:hold:worker`
- `looper:hold:fixer`
- `looper:hold:reviewer`

Semantics:

- `looper:hold` blocks all automatic Looper activity for the labeled issue or PR.
- lane-specific hold labels block only their lane.
- no issue/PR inheritance exists.
- Looper never adds or removes hold labels.
- removing a hold takes effect on the next normal scan.
- only explicit manual `looper work/review/fix --force` or API create requests with `force=true` can bypass hold.

Planner is special: only `looper:hold` blocks planner. There is no planner-specific hold label.

Manual CLI/API create-time hold validation is best-effort only when the local project repo path or configured `gh` path needed for remote inspection is unavailable. If those are present but `gh` inspection itself fails, create-time validation fails.

Coordinator example:

```toml
[roles.coordinator]
enabled = true
pollInterval = "5m"

[roles.coordinator.triage]
triagedLabel = "triaged"
maxIssueAgeDays = 7
maxPerTick = 5

[roles.coordinator.triage.disposition]
outOfScopeLabel = "wontfix"
unclearLabel = "needs-info"
reTriageOnAuthorReply = true

[roles.coordinator.dispatch]
mode = "human-gated"
assignTo = ""

[roles.coordinator.dispatch.humanGate]
slashCommands = ["/plan", "/implement"]
allowedUsers = []

[roles.coordinator.dispatch.autonomous]
delayMinutes = 30
holdLabel = "looper:hold"

[roles.coordinator.postMergeDigest]
enabled = true
schedule = "18:00"
timezone = "Asia/Taipei"
includeEmpty = false
maxItems = 50
```

Reviewer is the main migration example:

- legacy top-level `reviewer.*` is compatibility input only
- legacy reviewer discovery paths such as `roles.reviewer.autoDiscovery`, `roles.reviewer.triggers.*`, and `roles.reviewer.specReview.*` are compatibility input only
- canonical reviewer discovery lives at `roles.reviewer.discovery.*`
- canonical reviewer behavior lives at `roles.reviewer.behavior.*`

Canonical reviewer example:

This is a standalone reviewer-only snippet. Do not paste it together with the full config example below as a single TOML file, or table headers such as `[roles.reviewer.behavior.reviewEvents]` would be duplicated.

```toml
[roles.reviewer]
instructions = "Review for correctness, regressions, and migration safety."

[roles.reviewer.discovery]
autoDiscovery = true

[roles.reviewer.discovery.triggers]
includeDrafts = false
requireReviewRequest = true
enableSelfReview = false
labels = []
labelMode = "all"

[roles.reviewer.discovery.specReview]
includeReviewingLabel = true
reviewingLabel = "looper:spec-reviewing"

[roles.reviewer.behavior]
scope = "changed_ranges"
publishMode = "single_review"

[roles.reviewer.behavior.loop]
enabledByDefault = true
quietPeriodSeconds = 60
minPublishIntervalSeconds = 300

[roles.reviewer.behavior.convergence]
maxConsecutiveUnproductive = 3
maxFixerAttemptsPerItem = 4
maxTotalRounds = 40
severityFloor = "non_blocking"

[roles.reviewer.behavior.reviewEvents]
clean = "APPROVE"
blocking = "REQUEST_CHANGES"

[roles.reviewer.behavior.nativeResume]
onHeadChange = false
reReviewPromptOnHeadChange = false
```

The reviewer defaults above are intentionally aggressive: clean reviews publish `APPROVE`, blocking reviews publish `REQUEST_CHANGES`, and `enableSelfReview` still defaults to `false`.

### Removed Reviewer auto-merge

Reviewer no longer holds merge authority and never calls `gh pr merge`. Legacy `roles.reviewer.autoMerge` input is accepted only so an upgraded daemon can report a precise migration error. If its effective `enabled` value is `true`, startup fails and directs the operator to `roles.gatekeeper.trust = "auto"` and `roles.gatekeeper.strategy`. A disabled legacy block is ignored only when it uses the old default `squash` strategy; `merge` or `rebase` fails startup so an upgrade cannot silently change merge behavior. Compatibility blocks are omitted from global and project config projections; delete them after upgrading.

## Telegram intake

Telegram intake lets you open work from a chat instead of the forge UI. A message
becomes a GitHub Issue; Triager and Planner then handle it exactly as they would
an Issue you filed by hand.

**Intake does not start a loop by itself.** Without Triager enabled for the
project, the Issue sits there until you label and assign it the usual way.

**Intake does not think about the request.** The message text goes into the Issue
body verbatim, with only the first line lifted as a title. Judging whether a
request is specific enough is Triager's job, and paraphrasing here would hide the
original wording from that judgement.

| Path | Purpose | Default | Validation |
| --- | --- | --- | --- |
| `intake.telegram.enabled` | Enables the intake poll lane | `false` | When `true`, the fields below are required |
| `intake.telegram.botTokenEnv` | Env var **name** holding the @BotFather token | — | A pasted token value is rejected: it is not a valid env name |
| `intake.telegram.allowedUserIds` | Telegram user ids permitted to open work | — | Must be non-empty and positive; a negative value is a chat id, not a user id |
| `intake.telegram.defaultProjectId` | Project a message with no `#` prefix goes to | — | Must name a project in `projects[]`, checked at startup |

There is deliberately no "allow all users" switch. An unrestricted intake bot
lets anyone who finds the bot handle queue agent runs against your repositories.

### Routing a message to a project

A message goes to `defaultProjectId` unless it starts with `#<projectId>`. The
token ends at any whitespace, so a newline works as well as a space:

```
sweeper 沒有回收任何 worktree     → defaultProjectId
#novel 第三章的段落間距壞了       → project "novel"
```

An unknown project id is **rejected with a reply**, not silently redirected to
the default — a typo must not file work against the wrong repository.

### Duplicates, and the trade-off behind them

Every Issue intake opens carries a stamp identifying the chat message that
created it, and intake searches for that stamp before creating anything. Telegram
redelivers any update it has not been told was handled, so this stamp — not a
local cursor — is what keeps a redelivered message from opening a second Issue.

GitHub has no idempotency key for issue creation and its search index is
eventually consistent, so this check is best-effort: a crash in the seconds
between creating an Issue and acknowledging the message can still produce a
duplicate. That is the deliberate direction of the trade-off. A duplicate Issue
is visible, carries the same stamp as its twin, and can be closed; a dropped
request is invisible, and the person who typed it has no way to know. Intake
therefore also stops acknowledging at the first failed message rather than
skipping past it.

Intake latency is at most one `scheduler.pollIntervalSeconds` (30s by default).
The lane runs inside the serial scheduler tick, so it does not long-poll — waiting
on Telegram would hold every discovery lane hostage for the duration.

### Setup

1. Create a bot with [@BotFather](https://t.me/BotFather) and copy the token.
2. Export it under the name you configured, and make sure the daemon's
   environment has it.
3. Find your numeric user id (message [@userinfobot](https://t.me/userinfobot)).
4. Configure and restart `looperd` — all `intake.*` fields are restart-bound.

```toml
[intake.telegram]
enabled = true
botTokenEnv = "TELEGRAM_BOT_TOKEN"
allowedUserIds = [123456789]
defaultProjectId = "looper"
```

## Merge Gatekeeper trust level (`roles.gatekeeper.trust`)

Merge Gatekeeper evaluates every open pull request against merge policy —
required checks, review state, **unresolved review threads**, hold labels,
mergeability, project policy — and writes a durable Gate report. The trust level
decides what it may do with that judgement.

| Level | Behaviour |
| --- | --- |
| `observe` (default) | Gate report only. Nothing is published, nothing is merged. |
| `advise` | Additionally publishes the verdict and every blocking reason on the pull request, so the decision costs one read instead of a re-investigation. The human still merges. |
| `auto` | Requires a completed Looper/Codex review for the current head, then publishes the `Looper Gatekeeper` status for that exact **pull request head SHA**. GitHub branch protection consumes the status; Gatekeeper never calls merge itself. |

`auto` has one required external authority: GitHub branch protection must require
the `Looper Gatekeeper` status context on the target branch. The status is the
enforcement point for Mergify's branch-protection queue injection on the PR head;
the local Gate report remains audit evidence only. If protection does not
require that context, Gatekeeper publishes a failing status and marks the PR
ineligible — but an *unrequired* failing status cannot stop merge, so operators
must add the required check before relying on `auto`.

**Known limitations at `auto`:**

At `auto`, Gatekeeper therefore **re-runs the complete evaluation** immediately
before merging and proceeds only if it still passes against the same head. A
cheaper head comparison would miss exactly the changes that invariant names. The
merge itself passes `--match-head-commit`, so the forge refuses if anything was
pushed in between — the decision cannot be applied to a commit it was not made
about.

Every attempt is recorded, refusals included, with the gates that blocked the
confirming pass. The merge is immediate rather than handed to GitHub's
auto-merge: auto-merge applies the decision later, by which time the evaluation
behind it is stale.

```toml
[roles.gatekeeper]
trust = "advise"
strategy = "squash"
```

`strategy` accepts `squash`, `merge`, or `rebase` and is used only at `auto`.
Project overrides use `projects[].roles.gatekeeper.trust` and
`projects[].roles.gatekeeper.strategy`.

### The owned comment and its lifecycle

At `advise` Looper owns exactly one comment on each pull request, identified by
its marker **and** its author, so a human quoting the marker is never rewritten.

| Transition | What happens |
| --- | --- |
| First verdict | The comment is created |
| Verdict changed | The comment is updated in place |
| Verdict unchanged | **Nothing at all** — no write, and no read either |
| Demoted to `observe` | The comment is retired: its body is replaced with a withdrawal notice, once |
| Duplicates found | The oldest survives and is updated; the rest are deleted |

"Unchanged means no read" is the part that matters on a busy repository. Whether
a verdict changed is decided from the previous Gate report in the local event log,
so a quiet pull request costs no forge calls — scanning the discussion to discover
there is nothing to do would cost a comment page per pull request per tick.

Retirement keeps the marker, so promoting back to `advise` reuses the same comment
instead of leaving a withdrawn one and adding a second. Duplicate reconciliation
picks the oldest comment id, which every evaluator computes identically — two
Looper instances racing therefore converge instead of leaving two contradictory
verdicts.

The verdict states the head it was evaluated at and says plainly that anything
changing afterwards invalidates it. That is not decoration: holds, reviews,
threads, and policy can all change without moving the head, which is why the Gate
report is audit evidence rather than merge authority.

### Migration from `roles.reviewer.autoMerge`

Gatekeeper is the only Looper merge authority. Replace an enabled legacy block
with an explicit Gatekeeper level and strategy; there is no silent mapping
because `observe`, `advise`, and `auto` deliberately grant different authority.
Reviewer continues to publish reviews, while Gatekeeper independently evaluates
holds, requested changes, unresolved threads, checks, mergeability, policy, and
the current head before acting.

## Deploy on merge (`roles.deployer`)

When a project's base branch moves, the deployer runs one configured command
**against a checkout of that exact commit** and reports the result. It is
agent-free: Looper does not interpret the command, judge success beyond its exit
status, or roll anything back.

| Path | Purpose | Default |
| --- | --- | --- |
| `roles.deployer.enabled` | Enables the lane | `false` |
| `roles.deployer.command` | Run with `/bin/sh -c` from the materialized checkout | — (required when enabled) |
| `roles.deployer.timeoutSeconds` | Bounds one deploy, and how long an unfinished one holds its commit | `900` |
| `roles.deployer.environment` | Extra environment for the command | none |

Project overrides use `projects[].roles.deployer.*`, which is the common case: a
deploy command differs per repository. Configuration is validated at startup —
enabling deploys with no command fails immediately rather than on the first merge.

### Exact-commit materialization

The command does **not** run in the project's own checkout. That directory holds
whatever it happens to hold — another branch, stale contents, uncommitted edits —
so running there and then recording the remote commit as deployed would make the
deployment record untrue.

Instead Looper fetches the exact commit, creates a detached worktree at it,
verifies `HEAD` really is that commit, runs the command there, and removes the
worktree afterwards. The checkout path is derived from the commit, so a retry
after an interruption repairs the same directory rather than accumulating new
ones.

### What counts as already deployed

**GitHub Deployments are the authority**, under the `looper` environment — the
same preference for forge-native state as the dependency gate (ADR-0004) and
auto-merge (ADR-0005).

| State | Action |
| --- | --- |
| No deployment | Deploy |
| `success` | Nothing |
| `failure` | **Nothing.** A deploy that fails tends to keep failing; retrying every tick turns one broken deploy into a stream. Re-running is your call |
| `in_progress`, within the window | Nothing — a deploy is running |
| `in_progress`, past twice the timeout | Deploy. The daemon that claimed it is gone, and refusing forever would strand the commit |

The deployment is created and immediately marked `in_progress` **before** the
command runs. Without that claim an interrupted deploy is indistinguishable from
one that never started. The abandonment window is the one case where Looper acts
on a record it did not finish writing, so it is bounded explicitly.

Because the unit of work is a commit rather than a pull request, several PRs
merging together produce **one** deploy of the resulting head.

### Output handling

A deploy command's output is the most credential-dense text this daemon handles:
tokens, signed URLs, connection strings. It is therefore written to a `0600` file
under `daemon.logDir/deploys/` and **never** placed in the notification body or
the deployment status description, both of which are published. The notification
carries the exit code, a compare link, and the log path.

Compare links use the repository's own host, so an enterprise install links to its
own domain rather than github.com.

## Project override rules

Project entries stay in `projects[]`, but any override-bearing config must mirror the same local shape it uses globally.

Project entries are split into:

- **project metadata**: `id`, `name`, `repoPath`, `baseBranch`, `worktreeRoot`, `personalProject`
- **project-scoped override config**: canonical override-bearing domains such as `roles.<role>...`
- **project-local role instructions**: `projects[].roles.<role>.instructions`

Project override rules:

- if a field is overrideable per project, the project path uses the same local canonical shape as the global path
- project overrides remain part of the config-file layer; they do not create a new precedence layer above environment variables or CLI flags
- omitted project fields inherit the effective global value
- project-local role instructions may be set to an empty string to clear inherited global role instructions for that project
- legacy project reviewer discovery paths are compatibility-only; canonical reviewer project overrides live under `projects[].roles.reviewer.discovery.*`

`projects[].personalProject = true` is an explicit opt-in for a personal repository. When Planner or Worker discovery requires the current-user assignee—including work reached through Coordinator dispatch—Looper lists the project's trigger-labelled issues and assigns the configured GitHub identity only when that identity authored the issue and no assignee exists. Coordinator itself only projects the configured trigger labels; the downstream Planner/Worker lane performs the same identity and no-existing-assignee checks and records the assignment in loop/queue audit metadata. Existing assignees and shared projects are left unchanged. Looper never infers this setting from repository ownership, and routed projects do not use this local self-assignment policy.

Canonical project override example:

```toml
[[projects]]
id = "looper"
name = "Looper"
repoPath = "/absolute/path/to/looper"
baseBranch = "main"
worktreeRoot = "/Users/you/.looper/worktrees/looper"

[projects.roles.worker.discovery]
autoDiscovery = false

[projects.roles.reviewer]
instructions = "Project-specific reviewer guidance"

[projects.roles.reviewer.discovery.triggers]
labels = ["needs-review"]
labelMode = "any"
requireReviewRequest = false
```

## Full canonical example

```toml
[server]
host = "127.0.0.1"
port = 17310
authMode = "local-token"
localToken = "replace-me"
# Optional; set when a reverse proxy, tunnel, or TLS terminator sits in front
# of the daemon. Must be a canonical absolute http(s) URL with a host — no
# path prefix, credentials, query, or fragment — and any trailing slash is
# normalized away.
# baseUrl = "https://looper.example.com"

[daemon]
mode = "foreground"
restartPolicy = "on-failure"
restartThrottleSeconds = 10
logDir = "/Users/you/.looper/logs"
workingDirectory = "/absolute/path/to/where/you/start/looperd"
shutdownTimeoutMs = 1000

[daemon.worktreeCleanup]
enabled = false
interval = "24h"
retentionDays = 7
maxPerTick = 10
includeOrphans = false
dryRun = true

[daemon.environment]
EXAMPLE_FLAG = "1"

[storage]
mode = "sqlite"
dbPath = "/Users/you/.looper/looper.sqlite"
backupDir = "/Users/you/.looper/backups"

[scheduler]
pollIntervalSeconds = 30
maxConcurrentRuns = 3
retryMaxAttempts = 5
retryBaseDelayMs = 5000

[agent]
vendor = "opencode"
model = "your-model-if-needed"

# Optional named identity presets (vendor + model only). See
# "Multi-role agent vendor and model" above.
# [agent.profiles.fast]
# vendor = "opencode"
# model = "cheaper-model"

[agent.params]
reasoning = "medium"

[agent.env]
OPENAI_API_KEY = "replace-me"

# Agent subprocesses inherit only execution-safe host variables (for example,
# PATH, HOME, locale, temporary/configuration directories, certificate paths,
# SSH_AUTH_SOCK, and LOOPER_CONFIG so agent-invoked looper commands resolve the same config).
# Add required credentials or tool-specific variables here.

[agent.nativeResume]
enabled = true

[agent.timeouts]
plannerIdleTimeoutSeconds = 600
plannerMaxRuntimeSeconds = 3600
workerIdleTimeoutSeconds = 900
workerMaxRuntimeSeconds = 10800
reviewerIdleTimeoutSeconds = 600
reviewerMaxRuntimeSeconds = 5400
fixerIdleTimeoutSeconds = 600
fixerMaxRuntimeSeconds = 7200

[logging]
level = "info"
maxSizeMB = 10
maxFiles = 5

[notifications]
inApp = true

[notifications.osascript]
enabled = true
soundForLevels = ["action_required", "failure"]
throttleWindowSeconds = 60

[disclosure]
enabled = true
includeAgent = true
includeOS = false

[disclosure.channels]
gitCommit = true
pullRequest = true
issueComment = true
reviewComment = true
inlineCommentVisible = true

[tools]
gitPath = "/usr/bin/git"
ghPath = "/opt/homebrew/bin/gh"
osascriptPath = "/usr/bin/osascript"

[[providers]]
id = "acme"
kind = "github"

[package]
distribution = "github-release"
autoMigrateOnStartup = true
requireBackupBeforeMigrate = false

[defaults]
baseBranch = "main"
allowAutoCommit = true
allowAutoPush = true
allowAutoApprove = true
allowRiskyFixes = false
openPrStrategy = "all_done"
addSnapshotMode = "async"

# `allowAutoApprove` is a legacy compatibility alias.
# Prefer `roles.reviewer.behavior.reviewEvents.clean = "APPROVE"` in new config.

[roles.coordinator]
enabled = false
pollInterval = "5m"

[roles.coordinator.triage]
triagedLabel = "triaged"
maxIssueAgeDays = 7
maxPerTick = 5

[roles.coordinator.triage.disposition]
outOfScopeLabel = "wontfix"
unclearLabel = "needs-info"
reTriageOnAuthorReply = true

[roles.coordinator.dispatch]
mode = "human-gated"
assignTo = ""

[roles.coordinator.dispatch.humanGate]
slashCommands = ["/plan", "/implement"]
allowedUsers = []

[roles.coordinator.dispatch.autonomous]
delayMinutes = 30
holdLabel = "looper:hold"

[roles.planner.discovery]
autoDiscovery = true

[roles.planner.triggers]
labels = ["looper:plan"]
labelMode = "all"
requireAssigneeCurrentUser = true

[roles.reviewer]
instructions = "Review for correctness, regressions, and migration safety."

[roles.reviewer.discovery]
autoDiscovery = true

[roles.reviewer.discovery.triggers]
includeDrafts = false
requireReviewRequest = true
enableSelfReview = false
labels = []
labelMode = "all"

[roles.reviewer.discovery.specReview]
includeReviewingLabel = true
reviewingLabel = "looper:spec-reviewing"

[roles.reviewer.behavior]
scope = "changed_ranges"
publishMode = "single_review"

[roles.reviewer.behavior.loop]
enabledByDefault = true
quietPeriodSeconds = 60
minPublishIntervalSeconds = 300

[roles.reviewer.behavior.reviewEvents]
clean = "APPROVE"
blocking = "REQUEST_CHANGES"

[roles.reviewer.behavior.nativeResume]
onHeadChange = false
reReviewPromptOnHeadChange = false

[roles.fixer.discovery]
autoDiscovery = true

[roles.fixer.discovery.triggers]
includeDrafts = false
authorFilter = "current_user"
labels = []
labelMode = "all"

[roles.worker.discovery]
autoDiscovery = true

[roles.worker.triggers]
labels = ["looper:worker-ready"]
labelMode = "all"
requireAssigneeCurrentUser = true

[[projects]]
id = "looper"
name = "Looper"
repoPath = "/absolute/path/to/looper"
baseBranch = "main"
worktreeRoot = "/Users/you/.looper/worktrees/looper"

[projects.validation]
commands = ["scripts/verify.sh"]

[[projects]]
id = "second-example"
name = "Second Example"
repoPath = "/absolute/path/to/second-example"
provider = "acme"
repo = "acme/second-example"

[projects.validation]
optOut = true

[projects.roles.worker.discovery]
autoDiscovery = false

[projects.roles.reviewer]
instructions = "Project-specific reviewer guidance"

[projects.roles.reviewer.discovery.triggers]
labels = ["team:alpha", "needs-review"]
labelMode = "any"
requireReviewRequest = false
```

## Migration guide

This refactor is a warning-only migration release.

- Looper does **not** ship a `looper config migrate` command on the stripped CLI.
- Looper does **not** rewrite, rename, convert, or delete user config files during startup.
- Loading legacy `~/.looper/config.json` emits one informational note per process telling users that `~/.looper/config.toml` is now the preferred default path.
- Accepted legacy config paths, legacy environment variable names, and legacy CLI flags still load during this release, but they emit actionable replacement guidance.

### Deprecated reviewer migration example

Deprecated legacy JSON:

```json
{
  "reviewer": {
    "scope": "changed_files",
    "publishMode": "single_review",
    "reviewEvents": {
      "clean": "APPROVE",
      "blocking": "REQUEST_CHANGES"
    }
  },
  "roles": {
    "reviewer": {
      "autoDiscovery": true,
      "triggers": {
        "requireReviewRequest": true
      },
      "specReview": {
        "reviewingLabel": "looper:spec-reviewing"
      },
      "instructions": "Review carefully."
    }
  }
}
```

Canonical replacement:

```toml
[roles.reviewer]
instructions = "Review for correctness, regressions, and migration safety."

# Optional per-role agent identity (profile and/or inline vendor/model).
# [roles.reviewer.agent]
# profile = "strong"

[roles.reviewer.discovery]
autoDiscovery = true

[roles.reviewer.discovery.triggers]
requireReviewRequest = true

[roles.reviewer.discovery.specReview]
reviewingLabel = "looper:spec-reviewing"

[roles.reviewer.behavior]
scope = "changed_files"
publishMode = "single_review"

[roles.reviewer.behavior.reviewEvents]
clean = "APPROVE"
blocking = "REQUEST_CHANGES"
```

### Deprecated project reviewer discovery example

Deprecated legacy JSON:

```json
{
  "projects": [
    {
      "id": "looper",
      "name": "Looper",
      "repoPath": "/absolute/path/to/looper",
      "roles": {
        "reviewer": {
          "autoDiscovery": true,
          "triggers": {
            "labels": ["needs-review"]
          }
        }
      }
    }
  ]
}
```

Canonical replacement:

```toml
[[projects]]
id = "looper"
name = "Looper"
repoPath = "/absolute/path/to/looper"

[projects.roles.reviewer.discovery]
autoDiscovery = true

[projects.roles.reviewer.discovery.triggers]
labels = ["needs-review"]
```

## Environment variables and CLI flags

```json
{
  "reviewer": {
    "reviewEvents": {
      "clean": "APPROVE",
      "blocking": "REQUEST_CHANGES"
    }
  }
}
```

Reviewer behavior matrix:

| Reviewer outcome | `reviewEvents.clean` | `reviewEvents.blocking` | GitHub event |
|---|---:|---:|---|
| `clean` | `COMMENT` | any | `COMMENT` |
| `clean` | `APPROVE` | any | `APPROVE` |
| `non_blocking` | any | any | `COMMENT` |
| `blocking` | any | `COMMENT` | `COMMENT` |
| `blocking` | any | `REQUEST_CHANGES` | `REQUEST_CHANGES` |
| legacy `actionable` | any | any | `COMMENT` |

One-off reviewer jobs can snapshot the policy into loop metadata so queued work is not affected by later daemon config changes:

```bash
# looper review  # removed — use discovery / dashboard
  --clean-review-event APPROVE \
  --blocking-review-event REQUEST_CHANGES
```

To restore the previous synchronous `project add` behavior for one command:

```bash
# register via POST /api/v1/projects or [[projects]] in config
```

To restore it by default for all project additions:

```json
{
  "defaults": {
    "addSnapshotMode": "full"
  }
}
```

### `projects[].validation` and legacy `defaults.validationCommands`

`projects[].validation.commands` is the mechanical gate the worker and fixer run in that project's prepared worktree before opening a PR or pushing. Each entry is executed with `/bin/sh -c` from the worktree root, in order, and the first non-zero exit fails the validate step; the run never advances to open-pr or push. Each command is bounded by the coding role's `agent.timeouts.*MaxRuntimeSeconds` value.

```toml
[[projects]]
id = "looper"
name = "Looper"
repoPath = "/absolute/path/to/looper"

[projects.validation]
commands = ["scripts/verify.sh"]

[[projects]]
id = "novel"
name = "Novel"
repoPath = "/absolute/path/to/novel"

[projects.validation]
commands = ["pnpm test", "pnpm build"]

[[projects]]
id = "fluenx"
name = "FluenX"
repoPath = "/absolute/path/to/fluenx"

[projects.validation]
optOut = true
```

Each project with a configured Worker or Fixer must choose commands or explicitly set `optOut = true`. `commands` and `optOut` are mutually exclusive. Missing, empty, blank, or ambiguous policies fail closed at daemon startup, catalog publication, project registration, and configuration reload. A stored pre-validation API project with no `repo` metadata is inert and may start without a project policy only so an operator can repair that legacy record; while it has neither a project policy nor legacy default commands, it is excluded from the runnable catalog, manual Worker/Fixer admission, and durable Worker/Fixer claims. A PATCH that adds `repo` must include `validation` in the same request only when no legacy `defaults.validationCommands` fallback is available. The project API accepts the same JSON shape as `"validation":{"commands":["make check"]}` or `"validation":{"optOut":true}`. Explicit opt-outs are emitted in project responses and logged at scheduler startup; they are not silently treated as successful test runs.

For migration, the old global list remains a temporary fallback for projects that do not yet have a `validation` block:

```toml
[defaults]
validationCommands = ["make check"]
```

Looper logs each project using this deprecated fallback. Move commands into every `projects[].validation.commands` block, verify the project-specific commands, then remove `defaults.validationCommands`. A project-level policy always wins over the legacy global list, including an explicit opt-out.

The command strings come from daemon configuration, but commands such as `make check` and `go test ./...` intentionally execute repository-controlled code. Looper therefore launches each command through the vendor-neutral [Sandbox Runtime](https://github.com/anthropic-experimental/sandbox-runtime) (`srt`) process boundary: network access is disabled, `HOME`/XDG/Go write caches are disposable, daemon credentials (`SSH_AUTH_SOCK`, `LOOPER_CONFIG`, forge/API keys) are omitted, and writes are limited to the worktree plus that disposable root. Tool directories on `PATH` and the existing Go module cache are read-only. Exactly `@anthropic-ai/sandbox-runtime@0.0.67` and its platform support tools are required when any project has validation commands enabled. Install the runtime and its complete `node_modules` dependency tree in a dedicated administrator-owned prefix, and install Node, `ripgrep`, and (on Linux) `bubblewrap`/`socat` in paths whose files and ancestor directories the daemon user cannot modify. User-writable npm/Homebrew installs are intentionally rejected so an authorized coding agent cannot replace the next restricted runner, and sandboxed execution is rejected when `looperd` runs as root. Ubuntu 24.04+ also needs SRT's documented AppArmor `userns` allowance (or `kernel.apparmor_restrict_unprivileged_userns=0`). If the runtime is missing or rejects the profile, validation fails closed rather than falling back to an unsandboxed shell.

When this gate is configured, Looper is the fetch-and-publish authority. The daemon prepares the worktree before execution, then launches the coding agent with a read-isolated permission profile and a disposable, allowlisted tool environment. The agent's parent process can still authenticate to its model provider, but model-invoked tools cannot read daemon `HOME`/`CODEX_HOME`, SSH agents, forge credentials, or unrelated host files; they can edit and commit only inside the prepared worktree and cannot use the network. Looper then pushes the exact commit SHA that passed validation. A later local `HEAD` change is not included in that push.

The gate is coupled to the agent vendor: it can only run on a vendor whose CLI can deny network access to its tool subprocesses while the parent keeps its model connection. Today that is `codex` (workspace-write sandbox with `network_access=false`, browser and web-search tools disabled). Every other vendor fails closed: `devin-experimental` is not yet supported because the captured Devin CLI evidence marks strict tool-network containment a no-go and no integration check has verified a supported CLI version denies a network probe from an exec tool. Because `projects[].roles.*.agent` bindings are unsupported, the effective vendor comes from `roles.worker.agent.vendor` / `roles.fixer.agent.vendor` (falling back to global `agent.vendor`), so config validation rejects at startup any project whose effective validation commands (authored `validation.commands` or the inherited `defaults.validationCommands` fallback) run while those roles resolve to an unsupported vendor. The fix is to switch the role's vendor or set that project's `validation.optOut = true`; without the startup check the same run would fail at every spawn attempt.

The trade-off is deliberate: the gated agent and repository-controlled validation cannot fetch new remote state, query the forge, or download dependencies while they run. Required tools must be on the daemon's `PATH`; dependencies must already exist in the worktree or approved read-only caches. Linked-worktree Git metadata and the discovered Go toolchain/GOROOT are mounted read-only so normal local Git and Go validation still work. Remote context must be present in the daemon-supplied prompt and prepared checkout. If a task needs fresh remote information, let the daemon rediscover/restart it rather than weakening the gate. The agent receives a non-secret local Git identity so local commits still work; remote credentials remain daemon-side authority. Validation uses the same internal sandbox boundary for every configured agent provider; sandbox implementation details are not exposed through provider configuration.

Validation policy is restart-bound. Repository CI and branch protection remain inputs to merge eligibility after publication; configure required checks, and use `roles.gatekeeper.trust = "auto"` only when Gatekeeper should re-evaluate those gates and merge immediately.

### `roles`

The `roles` section controls scheduler-driven auto-discovery for planner, reviewer, fixer, and worker. It does not block manual commands, direct processing, retries, or already queued work.

Defaults preserve Looper's historical behavior:

- planner discovers open issues labeled `looper:plan` assigned to the current GitHub user
- worker discovers open issues labeled `looper:worker-ready` assigned to the current GitHub user
- reviewer discovers open non-draft PRs where the current user is requested for review, skips self-authored PRs by default, and includes the `looper:spec-reviewing` follow-up path
- fixer discovers open non-draft PRs authored by the current user that have actionable review items

Common fields:

- `roles.<role>.autoDiscovery`: when `false`, the scheduler skips new discovery for that role only
- issue roles (`planner`, `worker`): `triggers.labels`, `triggers.labelMode` (`all` or `any`), and `triggers.requireAssigneeCurrentUser`
- reviewer: `triggers.includeDrafts`, `triggers.requireReviewRequest`, `triggers.enableSelfReview`, `triggers.labels`, `triggers.labelMode`, `specReview.includeReviewingLabel`, `specReview.reviewingLabel`
- fixer: `triggers.includeDrafts`, `triggers.authorFilter` (`current_user` or `any`), `triggers.labels`, `triggers.labelMode`

Trigger fields are combined with logical AND. Label lists use `labelMode=all` or `labelMode=any`; an empty labels list means no label constraint.

When reviewer `triggers.requireReviewRequest=true` and no reviewer label filter is configured, discovery queries the forge for PRs review-requested from the current user. This avoids missing requested reviews that fall outside the generic open-PR discovery window.

For reviewer discovery, `triggers.enableSelfReview` defaults to `false`. When omitted or falsy, non-manual reviewer loops skip pull requests whose normalized PR author login matches the current authenticated GitHub login. Set it to `true` to allow those loops to review self-authored PRs.

Canonical environment variables and CLI flags override the config-file layer. Legacy names remain accepted only as compatibility aliases during the migration window.

Examples:

```bash
LOOPER_CONFIG="$HOME/custom-looper/config.toml" \
LOOPER_PORT=4321 \
LOOPER_TOKEN=replace-me \
LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_ENABLE_SELF_REVIEW=true \
looperd
```

```bash
looperd \
  --config "$HOME/custom-looper/config.toml" \
  --port 4321 \
  --roles-reviewer-discovery-triggers-enable-self-review=true
```

## Validation rules and startup failures

`looperd` fails fast on invalid config. Common validation rules:

- required strings must be non-empty
- numeric fields must be positive integers where applicable
- `server.port` must be between `1` and `65535`
- `scheduler.pollIntervalSeconds` must be at least `10`
- `authMode=local-token` requires `server.localToken`
- `authMode=none` requires `server.host` to be `localhost` or a literal loopback IP; all other binds require `local-token`
- `projects[].id` must be valid and unique
- `storage.dbPath` parent directory must be writable
- `daemon.logDir` must be writable
- `daemon.workingDirectory` must be writable
- the default worktree root must be writable
- required tool paths must resolve
- `notifications.osascript.enabled=true` requires `tools.osascriptPath` to resolve
- every `defaults.validationCommands[]` entry must be a non-empty string
- every project with a configured Worker or Fixer must set non-empty `projects[].validation.commands`, inherit the deprecated global fallback, or explicitly set `projects[].validation.optOut = true`
- `projects[].validation.commands` and `projects[].validation.optOut = true` cannot be combined
- effective validation commands (authored `projects[].validation.commands` or the inherited `defaults.validationCommands` fallback) require the effective Worker/Fixer vendor to support tool-network denial (`codex` only; `devin-experimental` is unsupported until integration evidence verifies exec-tool network containment)

## Recommended first-time setup

1. Install `git` and `gh`
2. Create `~/.looper/config.toml`
3. Add at least one project in `projects`
4. Set coding-role agent identity: either global `agent.vendor`, or `agent.profiles` / `roles.<role>.agent` bindings (see [Multi-role agent vendor and model](#multi-role-agent-vendor-and-model))
5. Start the daemon with your installed `looperd` (or `go run ./cmd/looperd` while developing)
6. Run `looper config show` to inspect the effective config

If you enable `server.authMode=local-token`, set `server.localToken` in the selected config or export `LOOPER_TOKEN`. The environment value has normal precedence over the file value for that process and is never persisted. Run `looper dashboard` and open the one-shot URL it prints to establish a browser session without putting the long-lived token in the URL.

## Troubleshooting

### `tools.gitPath` or `tools.ghPath` could not be resolved

Set explicit paths in the config file, or make sure the binaries are on `PATH` for the environment that starts `looperd`.

### `tools.osascriptPath is required when osascript notifications are enabled`

Either:

- install or expose `osascript`, or
- disable macOS notifications with:

```toml
[notifications.osascript]
enabled = false
```

### A runtime path is not writable

Make sure the daemon user can write to:

- the parent directory of `storage.dbPath`
- `daemon.logDir`
- `daemon.workingDirectory`
- the default worktree root under `~/.looper/worktrees`

## Worktree cleanup

Looper records worktrees it creates for planner, reviewer, fixer, and worker loops. The daemon periodically inspects those Looper-managed records and removes only clean worktree checkouts that are no longer referenced by active loop state.

Defaults:

- `daemon.worktreeCleanup.enabled = true`
- `daemon.worktreeCleanup.interval = "24h"`
- `daemon.worktreeCleanup.retentionDays = 7`
- `daemon.worktreeCleanup.maxPerTick = 10`
- `daemon.worktreeCleanup.includeOrphans = true` — a worktree record that no loop, run, or queue item references is still gated by `retentionDays`, because the planner ages every candidate by its own `createdAt`/`updatedAt`. Set this to `false` only to keep unreferenced worktrees indefinitely; the sweeper then reclaims almost nothing, since the reference graph drops old worktrees as loops move on.
- `daemon.worktreeCleanup.dryRun = false`

To disable automatic cleanup:

```toml
[daemon.worktreeCleanup]
enabled = false
```

To keep automatic inspection enabled without deleting anything:

```toml
[daemon.worktreeCleanup]
enabled = true
dryRun = true
```

Manual inspection is always dry-run by default:

```bash
looper worktree cleanup
looper worktree cleanup --dry-run
```

Run one immediate cleanup pass with the same safety rules:

```bash
looper worktree cleanup --confirm
looper worktree cleanup --json
```

Cleanup removes Looper-managed worktree checkouts only. It does not delete branches, skips dirty worktrees, preserves worktrees referenced by active loop state, and does not automatically delete filesystem-only orphan directories that are not present in Looper's worktree records.
