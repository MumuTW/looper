# Forgejo provider support

## Scope note

The MVP target is **Forgejo only**. Gitea is explicitly out of scope for this milestone.

Forgejo is a hard fork of Gitea and currently shares most of the `/api/v1` REST surface, so the implementation is written against that surface in a way that is likely reusable for Gitea later. But the MVP does not ship, configure, validate, or test a `gitea` provider kind. A future `gitea` provider is mentioned only where it affects how the abstraction is shaped today; it is not a deliverable here.

## Background

Looper currently treats GitHub as the workflow authority. The daemon and CLI use local `git` for repository and worktree operations, but use GitHub-specific surfaces for issues, pull requests, labels, review requests, review threads, branch protection, auto-merge, webhook forwarding, and user identity.

The most concentrated integration point is `internal/infra/github/gateway.go`, which shells out to `gh` for both high-level commands and raw API access:

- `gh pr list/view/create/edit/review/merge`
- `gh issue list/view/comment/close`
- `gh label create/edit/list`
- `gh api repos/...`
- `gh api graphql`
- `gh webhook forward`
- `gh auth status` and `gh api user`

The role runners already depend on gateway interfaces, but those interfaces are still named and shaped around GitHub. Some runners also import `internal/infra/github` types directly. This makes Forgejo support possible without rewriting the whole daemon, but not possible as a drop-in replacement for `gh`.

Forgejo exposes a Gitea-compatible REST surface, so a REST-backed implementation covers much of the basic issue/PR surface. Forgejo must still be modeled as a provider with explicit capabilities, not assumed to be identical to GitHub, because Forgejo is now a hard fork and its API details and workflow semantics differ from both GitHub and upstream Gitea.

## Goals

- Support Forgejo repositories without requiring `gh`.
- Preserve the existing GitHub behavior and `gh`-backed implementation.
- Keep `git` as the local repository/worktree/commit/push backend.
- Introduce a provider boundary that lets role runners depend on Looper workflow concepts instead of GitHub-specific command output.
- Ship a narrow first version that supports useful planner/worker flows (reviewer reduced, see "Reviewer") before attempting feature parity with GitHub.
- Make provider capability differences explicit in config validation, runtime status, and tests.

## Non-goals

- Do not replace GitHub support or remove `gh` in the first provider PR.
- Do not ship a `gitea` provider kind in this milestone.
- Do not promise GitHub-equivalent behavior for Forgejo review threads, auto-merge, branch protection, dependency gates, or webhook tunnel mode in the first version.
- Do not introduce another persisted authority layer to paper over missing forge features.
- Do not use `tea` or another forge CLI as the primary integration boundary unless a later design proves it gives better stability than REST.
- Do not change agent execution, worktree safety, or run recovery semantics except where the provider boundary requires type renames.

## Design principles

- Provider selection is explicit. A project must resolve to one provider: `github` or `forgejo`.
- **Config is the single authority for provider selection and repo identity.** Provider and repo are read from config at startup. They are not persisted as an independent authority that could drift from config. See "Repo identity and authority".
- Git remains separate from forge state. `git` owns local refs and worktrees; the provider owns issues, PRs, comments, labels, reviews, webhooks, and identity.
- Capabilities are declared from a static per-provider-kind table for the MVP. They are not inferred from failed operations and not derived from runtime version probing in the MVP. Version probing, if ever added, is a separate design with its own authority section (see "Risks").
- Provider differences should surface as disabled features with clear messages, not partial behavior that silently drifts.
- Delete GitHub-specific assumptions from role code only when they are being replaced by a narrower role-level contract.

## Target architecture

### Provider model

Add a provider package, for example `internal/forge` or `internal/vcs`, with role-facing contracts and shared value types.

Provider identity (host-level) and the repo binding (project-level) are separate concepts and should not be conflated in one struct:

```go
type ProviderKind string

const (
	ProviderGitHub  ProviderKind = "github"
	ProviderForgejo ProviderKind = "forgejo"
	// ProviderGitea is intentionally not defined in this milestone.
	// A future Gitea provider would add it here.
)

// Host-level provider definition (from config `providers`).
type ProviderConfig struct {
	ID      string // config key, e.g. "forgejo-main"
	Kind    ProviderKind
	BaseURL string
}

// Project-level binding of a repo to a provider (resolved from a project entry).
type RepositoryRef struct {
	ProviderID string       // -> ProviderConfig.ID
	Kind       ProviderKind // denormalized for convenience
	BaseURL    string       // denormalized for convenience
	Repo       string       // owner/name within BaseURL
}
```

The provider registry is keyed by `ProviderConfig.ID`; the repo lives on the project binding. Do **not** key provider instances only by resolved `(kind, baseUrl)`: two configured providers may intentionally point at the same Forgejo host with different `tokenEnv` values, and host-keying would let one project's identity lookup or REST mutation run with another provider's credentials. Host-qualified repo strings may remain supported for legacy GitHub compatibility, but new Forgejo config must set explicit `baseUrl` and `repo`.

### Repo identity and authority

This is load-bearing and must be decided before implementation, because the current codebase keys loop records, queue items, locks, PR snapshots, and recovery paths on a bare `repo` string (`owner/name`). Once the same `owner/name` can exist on two hosts (`github.com/acme/foo` and `code.example.com/acme/foo`), a bare `repo` is no longer a unique identity.

Decisions for the MVP:

- **For Forgejo projects, config is the authority** for provider and repo. A Forgejo project requires explicit `provider` and `repo` in config; they are resolved at startup and never read back from metadata. There is no config-vs-metadata reconciliation because there is no second source.
- **Legacy GitHub projects keep their current authority (transitional).** Today `ProjectRefConfig` has no `provider`/`repo` field (`internal/config/types.go:598-608`), and repo is autodetected and preserved in project metadata during `SyncConfigured` (`internal/projects/service.go`). The MVP does **not** rewrite that path. GitHub repo authority remains autodetect/metadata until a separate migration moves all projects to explicit config `repo`. This exception is explicit and bounded: it applies only to the existing GitHub provider.
- **Canonical project identity is the project `id`**, not the `repo` string. The project `id` is already unique in config and storage and is unaffected by host collisions.
- **For the MVP, two active projects may not resolve to the same `repo` regardless of provider/host.** Bare `repo` is rejected as a duplicate across configured projects and active stored projects, because loop records, queue items, locks, PR snapshots, and recovery are keyed on bare `repo`; allowing two projects on the same `repo` (even same host) would let them share repo-scoped locks/snapshots and double-watch the same remote PR. `SyncConfigured` must validate the incoming effective config against active projects already in storage, including GitHub projects that are no longer present in `cfg.Projects` but have not been archived. A collision is rejected at sync/startup; silently relying on config-only duplicate checks is not sufficient. This is stricter than necessary for cross-host safety but keeps the existing `repo`-keyed storage correct with no migration.
- **`baseUrl` must be normalized before use in validation and registry keys:** strip trailing slash, lowercase scheme+host, apply default ports, and reject an empty path-only value. GitHub host-qualified compatibility strings normalize to the GitHub provider.
- A host/provider-qualified repo key (for example `kind|baseUrl|owner/name`) is the eventual canonical form, but introducing it touches storage keys, lock keys, queue identity, and recovery. That migration is explicitly deferred and is not required by the MVP because of the duplicate-rejection rule above.

Authority-bearing behavior must name the authority per provider. Examples:

- Forgejo project provider/repo selection authority: config.
- Legacy GitHub project repo authority: autodetect/metadata (transitional, see above).
- Planner and worker issue discovery authority: provider issue state, labels, and assignees.
- Worker issue-selection authority: provider assignee membership. Forgejo worker does not claim by adding itself as an assignee; it only processes issues already assigned to the current user (see "Worker").
- Reviewer discovery authority: review request (GitHub) or a required non-empty label (Forgejo). Forgejo reviewer does not use review requests and never discovers match-all.
- Reviewer comment idempotency authority: Looper loop/run state keyed by PR head SHA (see "Reviewer"). This is idempotency state, not provider authority.
- Auto-merge authority: provider-native auto-merge/mergeability only when the provider exposes an equivalent signal (out of MVP scope).

If a provider lacks a durable native signal, do not invent one unless the PR explicitly justifies the new concept, its cost, and why deletion or feature disablement is insufficient.

### Capability model

Capabilities come from a static table keyed by `ProviderKind`. They are not probed at runtime in the MVP.

Two kinds of capability information are needed, and they should not be conflated:

1. **Presence flags** — whether a surface exists at all. These are booleans.
2. **Strategy/semantics** — *how* a behavioral surface works on this provider. These are enums, because the difference is behavioral, not on/off. Modeling them as bools pushes `if kind == ...` branching into role code and invites a hidden inference layer.

```go
type Capabilities struct {
	// Presence flags
	Issues           bool
	PullRequests     bool
	Labels           bool
	Assignees        bool
	ReviewRequests   bool
	BranchProtection bool
	AutoMerge        bool
	Checks           bool
	IssueDependencies bool

	// Behavioral strategies
	IssueClaimStrategy        IssueClaimStrategy        // worker issue claim
	ReviewerDiscoveryStrategy ReviewerDiscoveryStrategy // how reviewer finds PRs
	ReviewPublishStrategy     ReviewPublishStrategy     // how reviewer publishes
	ReviewThreadStrategy      ReviewThreadStrategy
	Webhook                   WebhookCapability
}

// Worker issue claim and reviewer PR discovery are different concepts and must
// not share one enum, or provider-kind branching leaks back into role code.
type IssueClaimStrategy string

const (
	IssueClaimByAssignee          IssueClaimStrategy = "assignee" // GitHub's existing claim behavior
	IssueClaimPreassignedAssignee IssueClaimStrategy = "preassigned_assignee" // only process issues already assigned to current user
	IssueClaimUnsupported         IssueClaimStrategy = "unsupported"
)

type ReviewerDiscoveryStrategy string

const (
	ReviewerDiscoverByReviewRequest ReviewerDiscoveryStrategy = "review_request" // GitHub
	ReviewerDiscoverByLabelAuthor   ReviewerDiscoveryStrategy = "label_author"   // Forgejo
)

type ReviewPublishStrategy string

const (
	ReviewPublishNative  ReviewPublishStrategy = "native_review" // reliable native PR review
	ReviewPublishComment ReviewPublishStrategy = "comment_only"
)

type ReviewThreadStrategy string

const (
	ReviewThreadNative      ReviewThreadStrategy = "native_resolve"
	ReviewThreadUnsupported ReviewThreadStrategy = "unsupported"
)

// WebhookCapability lists the webhook modes a provider supports. It does not
// pick a mode; the per-project webhook config does. GitHub keeps its existing
// modes (gh-forward, tunnel); Forgejo only polls in the MVP.
type WebhookCapability struct {
	SupportedModes []WebhookMode // reuses existing config.WebhookMode
	PollingOnly    bool          // true for Forgejo MVP: no webhook mode selectable
}
// WebhookManagedREST (create/update/delete hooks via REST) is deferred; see Phase 5.
```

GitHub's `WebhookCapability` lists its existing modes (`gh-forward`, `tunnel`) so validation/scheduling never regress current GitHub webhook configs. Forgejo's `WebhookCapability` is `{PollingOnly: true}` with no selectable mode.

Exact MVP capability values (out-of-MVP Forgejo features are set to false/unsupported even if the live server may expose them, because the MVP does not exercise them):

| field | GitHub | Forgejo (MVP) |
|---|---|---|
| Issues / PullRequests / Labels / Assignees | true | true |
| ReviewRequests | true | false |
| BranchProtection | true | false |
| AutoMerge | true | false |
| Checks | true | false |
| IssueDependencies | true | false |
| IssueClaimStrategy | assignee | preassigned_assignee |
| ReviewerDiscoveryStrategy | review_request | label_author |
| ReviewPublishStrategy | native_review | comment_only |
| ReviewThreadStrategy | native_resolve | unsupported |
| WebhookCapability | modes: gh-forward, tunnel | polling-only |

The capability table is used by config validation and scheduling. If a project enables a feature whose strategy is `unsupported` for its provider, startup fails with a clear config validation error rather than running with partial semantics. Because the GitHub defaults enable several of these features by default, Forgejo projects rely on a provider profile that overrides those defaults (see "Forgejo provider profile") rather than requiring users to disable each feature manually.

Authority statements live in "Repo identity and authority"; capabilities only describe what each provider can do, not which signal is authoritative.

### Config changes

Add provider config without breaking existing configs:

```json
{
  "providers": {
    "github": {
      "kind": "github",
      "baseUrl": "https://github.com",
      "ghPath": "/opt/homebrew/bin/gh"
    },
    "forgejo-main": {
      "kind": "forgejo",
      "baseUrl": "https://code.example.com",
      "tokenEnv": "LOOPER_FORGEJO_TOKEN"
    }
  },
  "projects": [
    {
      "id": "example",
      "name": "Example",
      "repoPath": "/repos/example",
      "provider": "forgejo-main",
      "repo": "acme/example"
    }
  ]
}
```

Compatibility:

- Existing GitHub projects without `provider` continue to use the default GitHub provider with their current autodetect/metadata repo authority (transitional; see "Repo identity and authority").
- Existing `tools.ghPath` remains valid and feeds the default GitHub provider.
- New Forgejo providers require `baseUrl` and either `tokenEnv`, `tokenPath`, or a future credential helper; Forgejo projects require explicit `name`, `provider`, and `repo`.
- Tokens must never be stored in project metadata or printed in diagnostics.
- Config validation rejects two active projects resolving to the same `repo`, regardless of provider/host, across both configured projects and active stored projects already known to Looper (see "Repo identity and authority").
- A Forgejo project gets the Forgejo provider profile applied to its effective role config (see "Forgejo provider profile"), so a minimal Forgejo project validates without the user disabling each GitHub-shaped default by hand.
- After the profile is applied, config validation rejects a Forgejo project that still enables any feature whose strategy is `unsupported` for Forgejo (native review, review-request discovery, reviewer match-all/empty-label discovery, thread resolution, fixer auto-discovery, coordinator, auto-merge, branch protection, dependency gates, routed network mode).

Open question: whether provider definitions belong under top-level `providers` or under an `integrations` namespace. Keep the first implementation simple unless there is a concrete conflict with existing config migration work.

### Forgejo provider profile

This resolves a concrete problem: the existing global defaults (`internal/config/defaults.go`) enable GitHub-shaped behavior that Forgejo does not support — reviewer `RequireReviewRequest: true`, reviewer events `APPROVE`/`REQUEST_CHANGES`, `PublishMode: SingleReview`, and `fixer.AutoDiscovery: true`. A minimal Forgejo project using the example config would otherwise fail validation or force users to override many fields by hand.

The MVP defines a **Forgejo provider profile**: a fixed compatibility profile — a set of effective-config overrides applied to a project once it resolves to a Forgejo provider. It is layered after hard defaults and before user global/project overrides, then validated with source information (which fields the user set explicitly). The capability table is not rich enough to express role policy (fixer, coordinator, reviewer label requirements), so the profile is a hand-maintained, fixed compatibility profile that must be *consistent with* (validated against) the capability table — not mechanically generated from it.

Config layering is source-aware so the profile can distinguish "default" from "explicit user opt-in" across every supported override source. This distinction must survive `looper config init`/generated config files: values serialized only because the default config writer emits the full default tree are treated as default-equivalent, not as intentional user overrides. Otherwise a stock generated config would re-enable GitHub-shaped defaults such as review-request discovery, native review events, or fixer auto-discovery before Forgejo validation runs.

1. hard defaults (`internal/config/defaults.go`)
2. Forgejo provider profile overrides (this section)
3. config file user global `roles.*` and per-project `roles` overrides
4. environment overrides
5. CLI flag overrides
6. validation

Validation runs against the merged result with knowledge of which fields the user set explicitly in the config file, environment, or CLI flags. Config-file values equal to generated defaults remain profile-overridable unless the user changed them away from the known generated value; env and CLI values are always explicit. A user explicitly enabling an unsupported capability on a Forgejo project from any of those sources (e.g. native review, fixer auto-discovery, review-request discovery, coordinator) is a validation error naming the unsupported capability. The profile does not silently override an explicit user opt-in into unsupported behavior; it only supplies safe Forgejo defaults where the user did not.

Forgejo profile (MVP):

- planner: enabled (unchanged).
- worker: enabled only for issues already assigned to the current user; the worker does not claim by mutating assignees.
- reviewer discovery: `RequireReviewRequest: false` and, for the normal single-token Forgejo setup where Looper-authored PRs are also reviewed by the same provider identity, `EnableSelfReview: true`. **Non-empty reviewer discovery labels are required for Forgejo, and spec-review and implementation-review discovery labels are distinct.** Because the current reviewer treats empty labels + `RequireReviewRequest:false` as match-all (`internal/reviewer/runner.go:1079`) and drops self-authored PRs when `EnableSelfReview:false`, the profile sets default labels, enables self-review visibility, and validation rejects a Forgejo reviewer with `AutoDiscovery:true` and no label for the PR phase being discovered. An implementation may instead support separate author/reviewer identities, but then validation and tests must prove worker/planner-authored PRs are still discoverable without relying on `EnableSelfReview`. Planner/spec PRs keep the existing spec-review label (`looper:spec-reviewing`) so `specpr.ResolvePullRequestPhase` continues to route them to spec-review instructions. Worker implementation PRs must apply a separate implementation-review discovery label and must not use `looper:spec-reviewing`, or they would be reviewed as specs. Worker PRs that are not labeled are not reviewer-discovered. The reviewer never discovers by review request on Forgejo.

Reviewer discovery label schema for Forgejo:

| PR phase | Config field | Forgejo profile default | Validation rule |
|---|---|---|---|
| Spec review | `roles.reviewer.discovery.specReview.reviewingLabel` with `includeReviewingLabel:true` | `looper:spec-reviewing` | Must be non-empty when spec-review discovery is enabled; this label is reserved for spec PRs. |
| Implementation review | `roles.reviewer.discovery.triggers.labels` with `labelMode:any` | `looper:impl-reviewing` | Must contain at least one non-empty label when Forgejo reviewer `autoDiscovery` is true; it must not equal the configured spec-review label. |

The Forgejo profile owns those defaults only when the user did not explicitly set the fields. If the config file, env, or CLI sets either field, validation uses that source-tracked value and fails fast if the resulting Forgejo reviewer would discover with an empty label, spec/implementation label collision, or review-request requirement.
- reviewer publish: forced to comment-only. `ReviewPublishStrategy: comment_only` overrides `PublishMode: SingleReview`; `ReviewEvents.Clean`/`ReviewEvents.Blocking` are not used to drive native review events. The reviewer emits a single PR/issue comment instead of a native `APPROVE`/`REQUEST_CHANGES` review.
- reviewer thread resolution: forced disabled (already default-disabled; profile asserts it).
- reviewer auto-merge: forced disabled (already default-disabled; profile asserts it).
- fixer: `AutoDiscovery` forced false, and Forgejo fixer queues are not scheduled or processed. (There is no `Enabled` field on `FixerRoleConfig` today; "fixer disabled" means no auto-discovery and no scheduling for Forgejo projects, not a new config field.)
- coordinator: not scheduled for Forgejo, and explicitly enabling coordinator on a Forgejo project is a validation error (not silently ignored).
- webhook: polling-only.

This profile is a new concept; its justification: it prevents a minimal Forgejo project from either failing validation against GitHub-shaped defaults or silently running GitHub-only behavior (match-all reviewer, native reviews, fixer) with no provider support. Its cost is one fixed per-provider override table plus source-aware config layering. The simpler alternative — making users hand-disable each GitHub default per project — was rejected because it is error-prone and the match-all reviewer failure mode is silent.

### Runtime lifecycle and recovery

Per-project provider resolution is not only a scheduler concern. The composition root currently builds and stores one global GitHub gateway and uses it for project sync, coordinator dependency validation, the recovery pipeline, deferred reviewer recovery, the network manager, and webhook setup (`internal/runtime/runtime.go:542-704`). "Do not change recovery semantics" is therefore insufficient on its own.

MVP lifecycle rules:

- A Forgejo-only install must start with **no** global GitHub gateway construction and no `gh` requirement.
- Recovery and deferred reviewer recovery are provider-aware: for Forgejo projects they recover only the roles enabled by the Forgejo profile (planner/worker, comment-only reviewer) and skip GitHub-only recovery (coordinator, native review/thread recovery).
- Coordinator dependency-gate validation runs only for GitHub projects.
- The network manager starts only when at least one routed GitHub project exists; routed mode is invalid for Forgejo.
- Webhook runtime applies only to projects whose provider exposes a selectable webhook mode (not `PollingOnly`).

These are covered by an acceptance test that boots a Forgejo-only install through the recovery path (not just a scheduler tick) with no `ghPath` configured.

### Storage changes

Avoid all storage migrations in the first version.

Current storage already uses generic fields such as `repo`, `pr_number`, and `metadata_json`. Provider/repo are resolved from config at startup, so the MVP **does not persist provider as an authority**:

- Do **not** add a persisted `provider` field as a source of truth. Config is the authority (see "Repo identity and authority"); a persisted provider field would create a second source that can drift from config.
- Loop records continue to use `repo` and `pr_number`. The duplicate-`repo` rejection rule keeps these unique without a schema change.
- Provider kind may be added to event payloads purely for diagnostics. Diagnostic event fields are not an authority and must never be read back to decide behavior.

Only add first-class DB columns after a concrete query or data integrity problem appears. A new persisted provider field is an authority-bearing schema change and requires the PR description to explain the failure it prevents, its cost, why config resolution is insufficient, and to pass `@oracle` review per repo design guidelines.

### Provider implementations

#### GitHub provider

The first GitHub provider should be a wrapper around the existing `internal/infra/github.Gateway`. It should preserve current behavior and command contracts.

This wrapper is intentionally transitional. It limits blast radius while role code migrates from GitHub-shaped interfaces to provider-shaped interfaces.

#### Forgejo provider

Implement Forgejo through REST over `net/http`.

Shared client responsibilities:

- Build URLs from `baseUrl` plus `/api/v1`.
- Send token auth via provider-specific config.
- Decode JSON using typed structs for stable fields.
- Preserve response bodies in sanitized error messages.
- Apply per-request timeouts.
- Support pagination.
- Normalize provider time formats into Looper's existing ISO strings where needed.

The client targets the Forgejo `/api/v1` surface. It is structured so a later Gitea provider could reuse it, but no `gitea` kind is wired up, validated, or tested in this milestone.

Do not shell out to `tea` for core behavior in the first implementation. A CLI dependency would create another command-output contract to test and would not remove the need to understand provider API semantics.

### Composition root, scheduler, and discovery

This is the area the original draft under-described, and it is where most of the real implementation cost lives. The current daemon is globally GitHub-shaped, not just inside role runners:

- The runtime composition root constructs a single concrete GitHub gateway and shares it across all projects (`internal/runtime/runtime.go`).
- The scheduler tick input is GitHub-shaped and builds GitHub adapters per tick (`internal/runtime/scheduler.go`).
- The scheduler builds GitHub-specific discovery snapshots and passes them into planner/worker/reviewer flows (`internal/runtime/scheduler.go`, `internal/infra/github/discovery_snapshot.go`, `internal/planner/runner.go`, `internal/worker/runner.go`).
- Project add/detect paths are GitHub-specific (`internal/projects/service.go`).
- Reviewer auto-merge validation is GitHub-specific (`internal/projects/reviewer_automerge_validation.go`).
- Webhook runtime/config assume `gh` (`internal/runtime/webhook.go`, `internal/runtime/webhook_forwarder.go`, `internal/config/validate.go`, `internal/cliapp/bootstrap.go`).
- Network routed identity is GitHub-specific (`internal/config/types.go`, `internal/networkpolicy/policy.go`).

MVP rules for this layer:

- **Provider is resolved per project, not globally.** The composition root builds a provider per project (or a provider registry keyed by config provider id) instead of one shared GitHub gateway. GitHub projects resolve to the transitional GitHub wrapper; Forgejo projects resolve to the REST provider.
- **The scheduler resolves the provider per project at tick time** and selects provider-specific role adapters. The scheduler must not assume a GitHub gateway.
- **Discovery snapshots are GitHub-only in the MVP.** The GitHub discovery snapshot is an optimization for the `gh`/GraphQL path. Forgejo projects run **without** a discovery snapshot in v1: planner/worker/reviewer make direct provider calls (list issues/PRs by label/assignee). Today role discovery inputs use the concrete `*githubinfra.DiscoverySnapshot` type. The MVP must keep that GitHub type **out of the new provider-neutral role interfaces**: either the snapshot stays a GitHub-only optional optimization behind the GitHub provider adapter (not in the shared contract), or Phase 1/3 removes it from role-facing signatures. Passing a `nil` GitHub snapshot through a still-GitHub-typed interface is not acceptable as the end state. A provider-neutral snapshot is explicitly deferred.
- **Bootstrap must not require `gh` for a Forgejo-only install.** Because the transitional GitHub provider wraps the current `gh`-backed gateway for core issue, PR, review, label, auth, and webhook operations, any install with a GitHub project still requires `gh` until GitHub has a non-`gh` provider. This is covered by an acceptance test (startup with no `ghPath` and only Forgejo projects).
- **`go-git` is not introduced.** Local git stays as-is; only forge API calls change.

## MVP scope

The first provider milestone should support:

- Project registration with explicit provider and repo (config-driven; see "Project registration").
- Current user identity.
- List/view open issues.
- List/view open pull requests.
- Add/remove labels.
- Add/remove assignees.
- Create/update issue comments.
- Create pull requests from pushed branches.
- Update PR title/body.
- Polling-based scheduling, with no discovery snapshot for Forgejo.
- Planner and worker roles end-to-end.
- A reduced reviewer that publishes a comment-only review (see "Reviewer").
- Contract tests against a fake Forgejo HTTP server.

The first milestone should not support:

- Native PR review publishing (reviewer is comment-only in MVP).
- Review thread auto-resolution.
- Reviewer/fixer ping-pong based on provider-native thread resolution.
- Fixer on Forgejo.
- Mutable Forgejo issue claim by label or by adding the current user as an assignee.
- Auto-merge.
- Branch protection gates.
- GitHub-style `blocked_by` dependency gates.
- Managed webhook tunnel mode.
- `gh webhook forward` equivalent.
- Network routed mode (explicitly invalid for Forgejo in MVP; rejected at config validation).
- Coordinator on Forgejo.
- A `gitea` provider kind.

### Project registration

Project registration is config-driven in the MVP. A Forgejo project is added by writing a `providers` entry plus a project entry with explicit `provider`, `repo`, and `repoPath` (see "Config changes"). There is no remote-URL autodetection for Forgejo in the MVP:

- `repo` (`owner/name`) is required explicitly for Forgejo projects.
- The GitHub autodetect path (`internal/projects/service.go`) is unchanged and remains GitHub-only.
- If `looper project add` is extended, it requires `--provider` and `--repo` for non-GitHub providers rather than guessing from the git remote. Remote-URL parsing for Forgejo is deferred.

## Role behavior

### Planner

Planner is the safest first role to enable.

Required provider methods:

- List open issues by label and assignee.
- View issue.
- Add issue assignee.
- List open PRs for dedupe.
- Create PR.
- Update PR body.
- Add PR labels.

Review requests are out of MVP scope. Planner creates the spec PR and labels it, and reports that automatic reviewer assignment is disabled for Forgejo. Discovery for Forgejo is direct (list issues/PRs by label/assignee), with no discovery snapshot.

### Worker

Worker is also in MVP scope.

Required provider methods:

- List open issues by label and current-user assignee.
- View issue.
- Create/update issue comments.
- Create PR.
- Compare branches or provide enough PR state for dedupe.
- Update PR title/body.
- Add/remove PR labels, including removing handoff labels and applying the implementation-review discovery label to PRs it creates (so the comment-only reviewer can discover them without making worker PRs look like spec PRs; see "Forgejo provider profile").

Forgejo worker does **not** claim work by adding the current user as an assignee. Forgejo/Gitea-compatible issues have a list of assignees, so adding the current user is not an exclusive worker claim: two daemon instances can both add themselves, both satisfy an assignee check, and both produce duplicate worker runs. The MVP therefore uses a pre-assigned invariant: a Forgejo worker only discovers and processes issues that already have the configured worker label and are already assigned to the current user before Looper starts work. If an issue is labeled but not pre-assigned to the current user, Forgejo worker skips it and may report it as not claimable.

The pre-assigned invariant is checked twice. Discovery only queues issues that currently have the worker label and current-user assignee, but queued and recovered worker items must re-view the issue immediately before any side effect such as commenting, branch creation, PR mutation, label mutation, or worktree execution. If the current user is no longer assigned at that claim-time recheck, the worker drops or defers the item without self-assigning and records a clear skip reason. This provider-specific path must bypass the existing GitHub-style self-assignment helper entirely; assignee mutation is not a fallback for Forgejo.

**Label-only claim is out of MVP scope.** A claim label is not an exclusive, race-free authority: two workers can both add the same label and both believe they own the issue. Supporting any mutable claim later, whether label-based or assignee-based, requires a separate design with a re-read/CAS-style invariant and concurrent-claim regression tests. If the pre-assigned invariant is too restrictive for a deployment, worker is disabled for that Forgejo project with a clear message rather than falling back to a racy mutation-based claim.

### Reviewer

Reviewer on Forgejo is **reduced to comment-only** in the MVP. Native PR review publishing, review request based discovery, and review-ID markers are out of scope until proven reliable on Forgejo.

Allowed first behavior:

- Discover PRs by a required non-empty label (and optionally author), never by review request and never match-all (see "Forgejo provider profile").
- View PR details and diff.
- Run the reviewer agent.
- Publish a single issue/PR comment (`ReviewPublishStrategy: comment_only`).
- Add/remove Looper labels.

Disabled first behavior:

- Native PR review publishing.
- Review-request based discovery.
- Thread resolution.
- Auto-approve plus auto-merge.
- Review marker lookup that depends on GitHub review IDs.

The reviewer publish target is fixed by the provider capability table (comment-only for Forgejo), not chosen at runtime. If a Forgejo project enables any disabled reviewer behavior, config validation fails with a clear unsupported-capability message.

**Comment idempotency.** Because Forgejo reviewer has no native review object and no thread/review-ID markers, repeated ticks/recovery/head changes must not spam duplicate comments. A strict "exactly one comment per head SHA, even across a crash" guarantee is **not** achievable on existing loop/run state alone: if the provider accepts the comment and the daemon crashes before the local publish record is written, recovery cannot know the comment exists without reading it back from the provider. The MVP does not add durable outbox/publish state for this. Instead it picks the cheapest honest rule:

- After a successful publish, the reviewer records the published head SHA in existing loop/run state and does not publish again for that head SHA. This is idempotency state, not provider authority, and must be labeled as such in code and the PR description.
- The guarantee is therefore: **no duplicate comment after a successful local publish record.** A crash in the narrow window between provider-accept and local-record can produce one duplicate comment on recovery. This is explicitly accepted for the MVP (a duplicate review comment is low-harm and visible), and called out in the PR description rather than hidden.
- On a new head SHA, a new comment may be published. The MVP does not edit/delete prior comments (editing a prior marked comment is a Phase 5 refinement).

If even this weaker rule cannot be implemented on existing loop/run state without new persisted fields, the reviewer is dropped from the Forgejo MVP rather than introducing a new persisted authority. In that case, goals, MVP scope, and acceptance criteria must drop the reviewer accordingly.

### Fixer

Fixer remains GitHub-only. It is out of scope for Forgejo in this milestone because it depends on review thread resolution semantics that Forgejo does not expose in a way Looper trusts.

A later Forgejo fixer can operate in a reduced mode by parsing unresolved review comments, but that must be a separate design because it changes the authority for "comment resolved".

### Coordinator and sweeper

Coordinator remains GitHub-only in this milestone because it uses issue dependencies, linked PRs, merge-watch, branch protection, and network control-plane behavior. Forgejo projects are not scheduled for coordinator, and a config that explicitly enables coordinator on a Forgejo project is rejected at validation with a clear unsupported-capability message (not silently ignored).

Sweeper is not part of this provider design. Issue #503 tracks removing sweeper; do not expand provider work to cover it.

## Webhooks

Provider support starts with polling. Webhooks are a second milestone.

GitHub's current webhook modes are tied to `gh`:

- `gh webhook forward`
- `gh api repos/{owner}/{repo}/hooks`
- GitHub signature and event headers

Forgejo webhook support should be implemented as:

- Provider REST calls for hook create/update/delete/list.
- Provider-specific signature validation and event parsing.
- Explicit event mapping into Looper queue events.

Until that exists, Forgejo projects must use polling (`WebhookCapability{PollingOnly: true}`) and status output should say webhooks are unsupported for that provider.

**Mixed GitHub + Forgejo installs.** Webhook config is global with per-project mode overrides today. The MVP rules:

- A GitHub project may use its existing webhook mode (`gh-forward`/`tunnel`).
- For a Forgejo project, an empty/unset project webhook mode means provider-default polling; no new `polling`/`disabled` config mode is added.
- Any explicit per-project webhook mode on a Forgejo project (`gh-forward`/`tunnel`, or any future selectable webhook mode before Forgejo supports it) is a validation error. It is never silently ignored, because the provider cannot honor it.
- Enabling global webhooks must **not** be rejected merely because a Forgejo project also exists.
- The global `gh` bootstrap requirement applies when at least one GitHub project exists, because the MVP GitHub wrapper still uses `gh` for non-webhook issue, PR, review, label, and auth operations. Forgejo-only installs do not require `gh`.

## Testing strategy

Add tests in layers:

- Unit tests for provider config normalization and validation, including `baseUrl` normalization, duplicate-`repo` rejection across configured and active stored projects (any provider/host), provider registry keying by `ProviderConfig.ID`, Forgejo-profile application, source-aware file/env/CLI unsupported-feature rejection, and unsupported-feature rejection.
- HTTP fake-server tests for Forgejo REST pagination, auth, errors, and field normalization.
- Role adapter tests that prove planner and worker use provider contracts rather than GitHub structs, and that they work with no discovery snapshot.
- Integration tests for config-driven project add, scheduler per-project provider resolution, planner publish, and worker PR creation with fake Forgejo responses.
- A startup test for a Forgejo-only install with no `ghPath` configured (proves no hidden global `gh` dependency).
- Existing GitHub `gh` contract tests remain unchanged for the GitHub provider.

Do not add real Forgejo sandbox E2E as a PR requirement for the first milestone. Add it later only for regressions that cannot be caught with contract tests.

## Migration plan

### Phase 0 - Inventory and names

Inventory must cover the whole composition, not just role imports:

- Role methods importing `internal/infra/github` (`internal/planner`, `internal/worker`, `internal/reviewer`, etc.).
- The composition root and shared gateway construction (`internal/runtime/runtime.go`).
- Scheduler tick input and adapter construction (`internal/runtime/scheduler.go`).
- Discovery snapshot construction and consumption (`internal/infra/github/discovery_snapshot.go`, `internal/planner/runner.go`, `internal/worker/runner.go`).
- Agent-facing prompt and fetch contracts that currently name GitHub or `gh` directly, including reviewer fetch instructions (`reviewerAgentSideGitHubFetchContract` in `internal/reviewer/runner.go`) and planner/worker prompts that describe GitHub issues, pull requests, labels, or review requests.
- Project add/detect/validate (`internal/projects/service.go`, `internal/projects/reviewer_automerge_validation.go`).
- Webhook and bootstrap `gh` assumptions (`internal/runtime/webhook.go`, `internal/runtime/webhook_forwarder.go`, `internal/config/validate.go`, `internal/cliapp/bootstrap.go`).
- Network routed identity (`internal/config/types.go`, `internal/networkpolicy/policy.go`).

Then:

- Create provider-neutral type names for issue, PR, review, label, and identity concepts.
- Rename role interfaces only where the provider-neutral replacement exists.
- Inventory each role prompt's mutable forge reads and side effects, then split provider-specific prompt/fetch-contract text so Forgejo agents are never instructed to run `gh pr view`, `gh pr diff`, `gh api`, or fail solely because `gh` is unavailable.

### Phase 1 - Provider config, registry, profile, and GitHub wrapper

- Add provider config with backward-compatible defaults.
- Build a GitHub provider wrapper over the current gateway.
- Move the composition root to per-project provider resolution (registry) instead of one shared GitHub gateway, including recovery, deferred reviewer recovery, network manager, and webhook setup (see "Runtime lifecycle and recovery").
- Add the Forgejo provider profile and the config-validation rules (duplicate `repo` across configured and active stored projects, unsupported-feature rejection with file/env/CLI source tracking, `baseUrl` normalization, provider registry keying by config provider ID).
- Make global `gh` detection conditional on at least one GitHub project existing; Forgejo-only installs do not require `gh`, but any GitHub project still does until GitHub has a non-`gh` provider.
- Keep all behavior green with `go test ./...`.

### Phase 2 - Forgejo client MVP

- Implement the Forgejo REST client over `/api/v1`.
- Implement issue, PR, label, comment, and identity methods.
- Add fake-server contract tests.

### Phase 3 - Planner and worker enablement

- Enable planner and worker for Forgejo projects with no discovery snapshot.
- Resolve provider per project in the scheduler.
- Add validation that unsupported provider features are rejected at config time.
- Migrate planner and worker prompts to provider-neutral wording for issues, PRs, labels, assignees, and review discovery. GitHub-specific prompt text remains only on the GitHub provider path.
- Document polling-only operation.

### Phase 4 - Reduced reviewer

- Add comment-only reviewer publishing for Forgejo.
- Replace the reviewer agent-side GitHub fetch contract with provider-specific fetch contracts: GitHub keeps the existing `gh`-based validation/diff/API instructions, while Forgejo uses provider-backed PR metadata and diff inputs supplied by Looper and never requires `gh`.
- Add prompt-contract regression tests that render Forgejo reviewer prompts and assert they do not contain GitHub-only `gh pr view`, `gh pr diff`, `gh api`, review-request, or native-review instructions.
- Keep fixer and coordinator disabled for Forgejo.

### Phase 5 - Optional advanced features

Only after the MVP is stable, and each gated behind a `gitea` decision where relevant:

- Native PR review publishing.
- Managed webhooks.
- Review thread resolution.
- Fixer support.
- Branch protection/checks.
- Auto-merge.
- Network routed mode.
- A `gitea` provider kind.

Each advanced feature must name the provider-native authority it relies on.

## Acceptance criteria

- Existing GitHub projects run unchanged, including their current autodetect/metadata repo behavior.
- `gh` is not required for Forgejo-only installs; any install with a GitHub project still requires `gh` until GitHub has a non-`gh` provider.
- A Forgejo project can be configured with explicit `name`, `provider`, `baseUrl`, token source, and `repo` (`owner/name`).
- Config validation rejects two active projects resolving to the same `repo` (any provider/host), including collisions between configured projects and active stored projects omitted from the current config.
- Config validation applies the Forgejo provider profile and then rejects Forgejo projects that still enable unsupported features (native review, review-request discovery, reviewer match-all/empty-label discovery, thread resolution, fixer auto-discovery, coordinator, auto-merge, branch protection, dependency gates, routed mode, label-only claim).
- A minimal Forgejo project using the documented example config validates without manual per-field overrides, and the resolved reviewer discovery has distinct non-empty labels for spec review and implementation review.
- Planner can discover an issue and publish a spec PR on a Forgejo project in fake-provider integration tests, with no discovery snapshot.
- Worker can process an issue that is pre-assigned to the current user, push code with `git`, and create/update a PR on a Forgejo project in fake-provider integration tests; tests prove a labeled but unassigned issue is skipped, a queued/recovered issue whose assignee was removed before claim-time side effects is skipped, and no self-assignment mutation is attempted or treated as an exclusive claim.
- Reviewer publishes a comment-only review on Forgejo and does not publish again for the same PR head SHA once a successful publish is recorded (a crash-window duplicate is accepted and documented), or fails validation with a clear unsupported-capability message if a disabled behavior is enabled.
- Forgejo planner, worker, and reviewer prompts are rendered from provider-specific contracts: reviewer prompts do not instruct agents to call `gh pr view`, `gh pr diff`, or `gh api`, and planner/worker prompts do not describe GitHub-only review-request or native-review flows.
- A Forgejo-only install boots through the recovery path (not just a scheduler tick) with no global GitHub gateway and no `ghPath`.
- A mixed config (GitHub project using `gh-forward`/`tunnel` webhooks + Forgejo project polling) validates and runs; existing GitHub tunnel/forward configs are not regressed.
- The new provider-neutral role interfaces do not reference the GitHub `DiscoverySnapshot` type.
- Unsupported features are visible in status/config validation and do not silently run with partial semantics.
- `go test ./...`, `go vet ./...`, and `go build ./...` pass.

## Risks

- Forgejo drifts from the Gitea-compatible surface after the hard fork. Mitigation: pin behavior to a static capability table per provider kind and cover it with fake-server contract tests. **Runtime version probing is not used in the MVP**; if introduced later it is a separate design with its own authority section (probing must not silently become the authority over the static table).
- Review thread semantics may not map cleanly. Mitigation: keep fixer and thread auto-resolution out of MVP; reviewer is comment-only.
- The composition is more GitHub-coupled than role runners alone. Mitigation: Phase 0 inventory covers the composition root, scheduler, discovery, bootstrap, and network identity, not just role imports.
- Provider abstraction may become too broad. Mitigation: start from role-required methods, not a full forge SDK.
- Config migration may overreach. Mitigation: default existing configs to GitHub and keep provider fields additive.
- Hidden `gh` dependency may remain in runtime paths. Mitigation: acceptance test that runs a Forgejo-only startup with no `ghPath`.

## Open questions

- Should provider credentials support only env vars initially, or also OS keychain/token files?
- When a later Gitea provider is added, should it be a distinct `gitea` kind or a shared `gitea-compatible` kind with Forgejo? (Not decided here; MVP ships `forgejo` only.)

## Resolved (previously open)

- **Forgejo vs Gitea kinds:** MVP ships `forgejo` only; `gitea` is deferred.
- **Comment-only reviewer:** it is a provider capability (`ReviewPublishStrategy`), not a per-reviewer config knob.
- **Repo autodetection for Forgejo:** not supported in MVP; `repo` (`owner/name`) is explicit config. GitHub autodetect is unchanged.
- **Network routed mode for non-GitHub:** explicitly invalid for Forgejo in MVP; rejected at config validation.
- **Repo/provider authority:** config is authority for Forgejo projects; legacy GitHub projects keep autodetect/metadata authority as a bounded transitional exception until a separate migration.
- **GitHub-shaped defaults vs Forgejo validation:** resolved by the Forgejo provider profile, which applies effective overrides before validation so a minimal Forgejo project validates.
- **Duplicate repo rule:** MVP rejects duplicate `repo` across all projects (not only across hosts), to keep existing `repo`-keyed storage/locks/recovery correct.
- **Claim enum:** split into `IssueClaimStrategy` (worker) and `ReviewerDiscoveryStrategy` (reviewer); no shared `ClaimStrategy`.
