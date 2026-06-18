# Steps

## Step 1

- Added provider-aware config schema in `internal/config` with `providers`, project `provider`/`repo` bindings, provider normalization, Forgejo-safe profile application, duplicate bare-repo validation, and Forgejo capability guards.
- Added initial `internal/forge` contracts for provider kinds, repository refs, static capabilities, and a registry.
- Verified with `go test ./internal/config ./internal/forge` and `go test ./...`; the full suite still reports an existing unrelated failure in `internal/cliapp` (`TestWebhookStatusVerboseShowsRuntimeDetails`).

## Step 2

- Made runtime GitHub gateway construction conditional on configured GitHub projects, while preserving legacy GitHub behavior for configs with no explicit provider.
- Made configured project sync prefer explicit `repo` values and avoid GitHub repo autodetection for non-GitHub projects; Forgejo-only startup/recovery now runs without `ghPath`.
- Kept GitHub discovery snapshots and existing role adapters GitHub-only for this step by skipping non-GitHub scheduler discovery until Forgejo role adapters land in later steps.
- Verified with `go test ./internal/config ./internal/projects ./internal/runtime`, `go vet ./...`, and `go build ./...`. `go test ./...` still reports the existing unrelated `internal/cliapp` failure in `TestWebhookStatusVerboseShowsRuntimeDetails`.

## Step 3

- Added `internal/forge/forgejo.go` with a Forgejo REST client that implements the provider contract, reads token auth from config/env, supports typed issue/PR/label/assignee/comment/identity methods, fetches PR diffs, applies request timeouts, and paginates via Forgejo response headers.
- Added fake-server contract coverage in `internal/forge/forgejo_test.go` for auth, pagination, typed decoding, sanitized error bodies, issue/PR reads, label and assignee mutations, issue comments, PR create/update, and config-driven client construction.
- Verified with `go test ./internal/forge`, `go test ./...`, and `go build ./...`. `go test ./...` still reports the existing unrelated `internal/cliapp` failure in `TestWebhookStatusVerboseShowsRuntimeDetails`.

## Step 4

- Enabled scheduler discovery for Forgejo planner and worker projects, while keeping unsupported non-GitHub coordinator/reviewer/fixer lanes explicitly skipped in this step.
- Finished provider-aware planner/worker runtime adapters in `internal/runtime/scheduler.go` so Forgejo projects can list issues, create PRs, mutate labels/comments, and resolve current-user identity through the existing REST client.
- Updated planner and worker prompt/contracts to use provider-aware issue wording, removed Forgejo-inappropriate GitHub CLI guidance, and changed Forgejo worker handling to require pre-assigned issues with an assignment re-check before side effects and no self-assignment fallback.
- Added focused coverage in `internal/runtime/scheduler_forgejo_test.go`, `internal/planner/runner_test.go`, and `internal/worker/runner_test.go` for Forgejo planner PR creation/labels, Forgejo worker PR creation, prompt text, and unassigned/de-assigned worker skip behavior.
- Verified with `go test ./internal/runtime ./internal/planner ./internal/worker`.

## Step 5

- Enabled Forgejo reviewer discovery in `internal/runtime/scheduler.go`, extended the reviewer adapter to use Forgejo label-filtered PR listing, PR metadata+diff fetches, snapshot capture, issue comments, and label removal through the REST client, and added comment-only publish mode routing into the reviewer runner.
- Updated reviewer prompt construction for Forgejo comment-only runs so the agent uses supplied metadata/diff context without GitHub CLI/native-review instructions, while the runner records `lastPublishedHeadSha` and publishes exactly one top-level comment per head.
- Added focused coverage in `internal/reviewer/runner_test.go` and `internal/runtime/scheduler_forgejo_test.go` for Forgejo comment-only prompt contracts, label-based discovery without review requests, local head-SHA idempotency after publish, and reviewer adapter Forgejo endpoints.
- Verified with `go test ./internal/forge ./internal/reviewer ./internal/runtime`.

## Step 7

- Updated `README.md`, `docs/configuration.md`, `docs/installation.md`, and `docs/users-guide.md` so public docs describe explicit `github`/`forgejo` provider support, conditional `gh` requirements, Forgejo-only boot behavior, and Forgejo MVP role limitations.
- Updated `skills/looper/references/config.md`, `skills/looper/references/daemon.md`, and `skills/looper/references/cli.md` so the bundled agent guidance matches Forgejo provider configuration, provider-aware startup checks, and Forgejo comment-only reviewer behavior.
- Verified with `git diff -- README.md docs/configuration.md docs/users-guide.md docs/installation.md skills/looper/references/config.md skills/looper/references/daemon.md skills/looper/references/cli.md` and a manual pass over the updated docs.
