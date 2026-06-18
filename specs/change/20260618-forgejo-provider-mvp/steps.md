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
