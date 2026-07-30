# Independent maintenance and release ownership

## Decision

As of 2026-07-29, `MumuTW/looper` is independently maintained. GitHub Releases under `MumuTW/looper` are authoritative for Looper CLI and daemon installation and upgrades.

## Runtime and publishing rules

- `looper bootstrap`, `looper daemon install`, and `looper upgrade` must resolve artifacts from `MumuTW/looper`.
- Install/takeover scripts, release manifests, feedback, disclosure links, skills, documentation, and GHCR examples default to `MumuTW`.
- The Git `upstream` remote may remain for reference or selective cherry-picks; it does not control runtime artifact downloads.
- Keep the Go module/import path `github.com/MumuTW/looper` until a separate, deliberate module-rename migration.
- Continue recognizing and removing legacy `nexu-io` and `powerformer` disclosure stamps so existing PR content remains normalized.

## Implementation

- PR: `MumuTW/looper#2`
- Commit: `4236ca5981171f9f8fadf041aa52e8f0283e72ad`
- Validation: `./scripts/verify.sh` passed (gofmt, vet, all Go tests, release-ldflags build).

## Release prerequisite

Before publishing the first independent release, create/configure `MumuTW/looper-sandbox` and provide `LOOPER_E2E_GITHUB_APP_ID` plus `LOOPER_E2E_GITHUB_APP_PRIVATE_KEY` for the sandbox preflight workflows.
