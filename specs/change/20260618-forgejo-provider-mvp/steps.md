# Steps

## Step 1

- Added provider-aware config schema in `internal/config` with `providers`, project `provider`/`repo` bindings, provider normalization, Forgejo-safe profile application, duplicate bare-repo validation, and Forgejo capability guards.
- Added initial `internal/forge` contracts for provider kinds, repository refs, static capabilities, and a registry.
- Verified with `go test ./internal/config ./internal/forge` and `go test ./...`; the full suite still reports an existing unrelated failure in `internal/cliapp` (`TestWebhookStatusVerboseShowsRuntimeDetails`).
