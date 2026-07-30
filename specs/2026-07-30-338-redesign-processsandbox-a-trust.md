# Spec: Content-addressed trust authority for the SRT runtime (#338)

- **Issue:** MumuTW/looper#338 — redesign(processsandbox): a trust authority for the srt runtime that survives hosted-runner filesystems
- **Base branch:** `main`
- **Type:** Trust-authority redesign of `internal/processsandbox` (replaces the ancestor-writability proxy)
- **Refs:** #313 (merged, insufficient), #323 (closed under the second-fix rule), #331 (closed, disproved by its own verify run), #318 (first live `/usr` counterexample)

## Problem

`requireTrustedTree` / `requireTrustedPath` (`internal/processsandbox/srt.go:352`, `:379`) approximate one invariant — *the daemon user must not be able to substitute the code the sandbox is about to execute* — by probing `access(W_OK)` on every ancestor of the srt tree and each support-tool binary. That proxy is broken in two independent ways, and three consecutive `fix:` PRs to the same subsystem have died in sequence, which per AGENTS.md ("a second fix to the same subsystem is a revert signal") means the abstraction itself is wrong rather than the patch.

1. **It under-covers the trust surface.** The probe walks the entrypoint's ancestor directories but never looks at the ELF interpreter (`ld.so`), the shared-library closure, or the `/usr/bin/env` interpreter of the srt launcher script. #331's closing analysis found the deeper hole: copying an ELF entrypoint into a root-owned prefix does not move its dynamic linker, its shared libraries, or the launcher's script interpreter, so a path probe can accept the prefix while the code that actually executes remains replaceable in writable `/usr`. The trust surface is the full interpreter/dependency closure, not the entrypoint's ancestor chain.
2. **It is unsatisfiable on hosted images.** Observed writable roots across runs on main-identical code: `/usr/local/lib`, `/usr`, `/opt`. The runner user also holds passwordless sudo, so the boundary the probe tries to enforce is unenforceable anyway — `chown`/`chmod`/`setfacl` on a few ancestors (#313) only moves the variance up the tree, and #323's extension to `/` plus #331's dedicated `/opt` prefix both failed because the prefix root itself was writable on the hostile image. No path/permission scheme can be complete here, because filesystem ownership varies by image build and the runner can `sudo` around any of it.

The combined effect: every PR's `verify` job is an image lottery. `LOOPER_REQUIRE_TRUSTED_SRT=1` (`.github/workflows/ci.yml:96`) exists to *enforce* that the trusted-SRT slice runs rather than skips, so the only current workaround is rerunning until the runner pool serves a permissive image. Option C (CI-only trust waiver) is rejected by the issue because it deletes exactly the coverage that env var exists to enforce.

## Goals

- Replace the ancestor-writability proxy with an authority that verifies the **complete executable closure** the sandbox will actually execute: the srt tree, each support-tool binary (`node`, `rg`, `bwrap`, `socat`), each binary's resolved dynamic dependencies (`ldd` closure including the ELF interpreter), and the srt launcher's script interpreter (`/usr/bin/env` or equivalent).
- Make substitution **detected** rather than assumed impossible, so writable ancestors stop mattering. The trust decision rests on content, not on filesystem ownership that varies per image.
- Anchor the manifest's own integrity in root ownership of **one file**, a much smaller surface than every ancestor of every binary in the closure.
- Retain #313's revert of global filesystem mutations inside the focused design (per #331's closing directive): no `chown`/`chmod`/`setfacl` sweeps of `/usr/local`, `/usr`, or `/opt` as a runtime trust mechanism. The harden-ancestors CI step is removed as a trust dependency; if any filesystem setup remains it is install-time only and not part of the trust contract.
- Name the authority and its stated gaps explicitly in the contract rather than implying a boundary it cannot hold.
- Pin the sudo-npm toolchain that #331 also flagged as unpinned.

## Non-goals

- Closing the TOCTOU window between verify and exec. That window exists in the current design too; this redesign states it, it does not eliminate it (see Risks).
- Defeating a CI user who holds passwordless sudo. If the runner can `sudo`, it can rewrite root-owned files including the manifest. The contract states this as a stated gap, not a property it pretends to hold.
- Container trust root (Option B). Deterministic, but bubblewrap inside an unprivileged container needs userns privileges the default docker seccomp/apparmor profile blocks, and the workflow surgery plus privilege flags are their own risk surface. Out of scope for this redesign.
- Changing the SRT containment policy itself (filesystem deny/allow lists, network egress, credential isolation). Only the *trust probe that gates whether the pinned runtime may run* changes.

## Authority

> **What is the authority for this action, and why is it not the daemon user's own structured output?**

The authority is a **root-sealed manifest** written at install time and verified at spawn time. The daemon user is the party the sandbox exists to constrain, so its own output cannot be the authority for "the code about to execute has not been substituted." Infra signals (ancestor writability) were a proxy for that authority and failed because they describe the filesystem, not the bytes that will execute. The manifest is content-addressed: SHA-256 over the actual executable closure, anchored in root ownership of one file. Writable ancestors become drift signals, not authority.

## Approach

Option A from the issue: content-addressed trust, with the authority revised explicitly.

### 1. Install time — root seals a manifest

A root-owned install step computes and writes a manifest covering the complete executable closure:

- The srt package tree (the `node_modules/@anthropic-ai/sandbox-runtime` root already located by `installedRuntime`).
- Each support-tool binary resolved by `exec.LookPath` + `EvalSymlinks`: `node`, `rg`, and on Linux `bwrap`, `socat`.
- For each ELF binary, the resolved dynamic dependency closure via `ldd` (or an equivalent parse of `.interp` + `DT_NEEDED`), **including the ELF interpreter** (`ld.so` / `ld-linux.so`).
- For the srt launcher script, its script interpreter (the shebang target, e.g. `/usr/bin/env` → resolved to `/usr/bin/env` itself).

Each entry is recorded as `{path, kind, sha256}`, where `kind` distinguishes tree-file / elf-binary / shared-lib / interpreter / script-interpreter so the verifier knows what it is checking. The manifest file itself is written root-owned, mode `0o644` (world-readable so the non-root daemon can verify) but **not** writable by the daemon user. Its own integrity anchors in root ownership of that one file. The manifest path is a fixed, install-time-known location (e.g. alongside the runtime prefix) and is located by an absolute path, not by a `PATH` search.

The install step also pins the sudo-npm toolchain (#331's unpinned-toolchain finding): the `npm`/`node` used to install `@anthropic-ai/sandbox-runtime@0.0.67` is pinned to a resolved version, not floated.

### 2. Spawn time — verify digests against the sealed manifest

`installedRuntime` (`internal/processsandbox/srt.go:258`) is revised. After locating the srt package and support tools as today, instead of (or in addition to, where the FS allows it) calling `requireTrustedTree`/`requireTrustedPath`, it:

1. Loads the sealed manifest from its fixed path. If the manifest is missing, unreadable, or not root-owned, trust fails — this is the small-surface integrity check that replaces the every-ancestor walk.
2. For each manifest entry, re-resolves the path the same way install did (same `LookPath` + `EvalSymlinks` + `ldd` closure logic, factored into a shared resolver so install and spawn cannot drift), hashes the bytes, and compares to the sealed digest. Any mismatch is a substitution and fails trust.
3. Confirms the closure is **complete**: every binary the spawn path will execute has a manifest entry. A binary with no entry (e.g. a support tool whose `ldd` closure changed) is a drift signal, not a silent pass.

`requireTrustedTree` and `requireTrustedPath` are **removed** as the trust authority. Ancestor writability may be retained as a *secondary* drift signal where the filesystem permits it (cheap, and on a well-behaved image it catches a different class of tampering than digests do), but it is no longer the gate that decides whether the pinned runtime may run. The euid-0 rejection stays — the probe rejects euid 0 in both the current code and the container-trust-root option, and it stays here.

### 3. CI workflow

- The "Harden sandbox runtime ancestors" step (`.github/workflows/ci.yml:88`) is **removed as a trust dependency**. It may be deleted entirely, or retained only as install-time setup that is not part of the trust contract — the spec prefers deletion, since #313's directive is to retain the *revert* of global mutations, not the mutations themselves.
- The "Install process sandbox runtime" step (`.github/workflows/ci.yml:70`) gains a root-owned manifest-sealing sub-step that runs after install and writes the manifest. This is the one new piece of CI surface.
- `LOOPER_REQUIRE_TRUSTED_SRT=1` stays and continues to enforce that the trusted slice runs rather than skips. The whole point of the redesign is to make that enforcement reliable across images, not to weaken it.

### 4. Contract documentation

The trust contract is documented in a comment on the revised `installedRuntime` / verifier and states explicitly:

- **Authority:** root-sealed content manifest, not ancestor writability and not daemon output.
- **Stated gap — TOCTOU:** there is a window between digest verification and `exec`. A daemon that can write the verified files in that window can substitute. This window exists in the current design too; the redesign does not widen it.
- **Stated gap — sudo-equivalence:** on a runner where the CI user holds passwordless sudo, root ownership is not a boundary the daemon cannot cross. The manifest raises the cost (rewrite a root-owned file and recompute SHA-256 over the closure) but does not bind a privileged user. This is stated, not hidden.
- **What it catches that the proxy did not:** substitution of `ld.so`, shared libraries, or the launcher's script interpreter — the closure the ancestor probe never looked at.

## Design trade-off (required by AGENTS.md)

> **Delete this six months from now — what breaks?**

Without the manifest, the trust probe goes back to either the ancestor-writability proxy (broken on hosted images, under-covers the closure) or no trust gate at all (the Option C waiver the issue rejects, which deletes `LOOPER_REQUIRE_TRUSTED_SRT` coverage). The concrete failure the manifest prevents is a substituted `ld.so` or shared library that the ancestor probe accepted because the entrypoint's prefix was root-owned while the interpreter in writable `/usr` was not (#331's exact failure). A simpler move — delete the trust layer entirely and trust agent output — is rejected because the sandbox's whole purpose is to constrain the daemon user, so its output cannot be the authority for "the code about to execute is unchanged."

> **What does it still not catch?**

The TOCTOU window between verify and exec, and a CI user with passwordless sudo who can rewrite root-owned files. Both are stated in the contract. It also does not catch a compromise of the install step itself (a malicious seal), which is true of any install-time authority and is bounded by the install step running in CI under review, not by the runtime probe.

Because the manifest is persisted state, the diff must contain the test that fails when that state goes stale: a test that seals a manifest, mutates a binary in the closure, and asserts the verifier rejects. Without that test the manifest's drift detection is an unverified guess.

## Risks

- **TOCTOU between verify and exec.** Stated, not closed. The window is no larger than the current design's; the contract names it. Mitigation is documentation, not a gate — adding a gate inside the window would be the kind of second-layer fix the second-fix rule warns against.
- **Sudo-equivalence of the CI user.** On a runner with passwordless sudo, root ownership is not a hard boundary. The manifest raises substitution cost (rewrite root-owned file + recompute closure digests) but does not bind a privileged user. Stated as a gap. Mitigation is the contract text, not a pretense that root ownership binds sudo.
- **Install/spawn resolver drift.** If install and spawn resolve the closure differently (different `ldd` output, different symlink resolution), the verifier rejects a legitimate install. Mitigation: factor the closure resolver into one shared function used by both paths; add a test that seals on the install resolver and verifies on the spawn resolver against the same installed tree.
- **Manifest staleness after a runtime upgrade.** Bumping `supportedRuntimeVersion` (`srt.go:237`) or any support tool without re-sealing makes every spawn fail trust. Mitigation: the CI install step re-seals on every run, and the staleness test (mutate-then-reject) is the regression coverage AGENTS.md requires for persisted state.
- **`ldd` availability / portability.** `ldd` is a Linux convention; on macOS the closure model differs (dyld, `.dylib`). The current code already branches on `runtime.GOOS` for support tools and read roots; the closure resolver does the same. macOS local-dev runs already fall through to the `LOOPER_REQUIRE_TRUSTED_SRT != 1` skip path, so a macOS closure resolver is only needed for local fidelity, not for CI enforcement.
- **Test-file growth smell.** The closure resolver + verifier is a pure, extracted function (out of the runner), which is the "covering" case from AGENTS.md — production shrinks because `requireTrustedTree`/`requireTrustedPath` and the ancestor walk are removed, and the new tests assert first-time coverage of the extracted logic. The spec names this: the surface shrinks by two functions and the remaining rows are the surviving behavior. If test growth instead multiplies into a wide matrix over closure kinds, that is the "propping up" signal and the resolver should be simplified, not the table widened.

## Validation

- **Unit — verifier rejects substitution:** seal a manifest over a temp install tree, overwrite one binary in the closure with same-path different bytes, assert the verifier fails with a digest-mismatch error. This is the persisted-state-staleness test AGENTS.md requires.
- **Unit — verifier rejects missing closure entry:** seal a manifest, add a new support tool not in the manifest, assert failure (drift, not silent pass).
- **Unit — verifier rejects non-root-owned manifest:** seal a manifest, chown it to the daemon user, assert the integrity check fails.
- **Unit — install/spawn resolver agreement:** seal via the install resolver, verify via the spawn resolver against the same tree, assert pass. Mutate a symlink so the two would diverge, assert the shared resolver prevents drift.
- **Contract/invariant integration — trusted slice runs across images:** with `LOOPER_REQUIRE_TRUSTED_SRT=1`, the existing `TestRunReadOnlyContainsMaliciousProcessTree` (`srt_test.go:96`) and the `validationcmd` sandbox runtime tests (`runner_test.go:136`) run against the manifest-sealed install and pass. This is the cross-component invariant coverage AGENTS.md prefers for runtime-path regressions, not unit-only.
- **CI — harden-ancestors step removed as a trust dependency:** verify no longer depends on the `chown`/`chmod`/`setfacl` step; the manifest-sealing sub-step is the new install-time authority. Re-run verify on a hostile image (writable `/usr/local/lib`, `/usr`, `/opt`) and confirm the trusted slice still passes — the image lottery is over.
- **No code regression:** `go vet ./...`, production-only `staticcheck`, `go test ./...`, and `go build ./...` remain green. `requireTrustedTree`/`requireTrustedPath` deletion is covered by the new verifier tests, not by leaving the old functions unreachable (staticcheck `U1000` would flag dead code).
- **Contract documentation:** the revised verifier's doc comment names the authority, the TOCTOU gap, and the sudo-equivalence gap. A reviewer can read the contract from the code, not from a separate doc.

## Out of scope / follow-ups

- **CONTRIBUTING / PR template note.** The issue asks that, if this stays open long, the image-lottery workaround (reruns) be stated in CONTRIBUTING or the PR template. Once this lands the lottery is resolved and the note is unnecessary; if the redesign slips, add a one-line rerun note to `.github/pull_request_template.md` under the E2E / Invariant Risk section as a interim mitigation, and remove it when this ships.
- **Container trust root (Option B).** Revisit only if content-addressed trust proves insufficient on a runner class the manifest cannot cover; the workflow privilege surgery is its own risk surface and is not bundled here.
- **Closing the TOCTOU window** (e.g. verify-from-inside-the-sandbox, or a kernel-assisted seal) is a follow-up design, not part of this redesign — it would re-introduce the layering the second-fix rule warns against if stacked on top of a half-working gate.
