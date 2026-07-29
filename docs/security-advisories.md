# Security Advisory Decisions

This file records decisions on dependency security advisories that `pnpm audit`
reports against this repository but that we have determined are not applicable
to Looper, or that we are otherwise tracking deliberately. Each entry is
time-bounded and must be revalidated by its stated date, or sooner if the
affected architecture changes.

## GHSA-qwww-vcr4-c8h2 — react-router RSC mode CSRF bypass (not applicable)

- **Advisory:** [GHSA-qwww-vcr4-c8h2](https://github.com/advisories/GHSA-qwww-vcr4-c8h2)
  — "React Router: RSC Mode CSRF Bypass Allows Action Execution Before 400
  Response" (follow-up to CVE-2026-22030). Severity: high.
- **Affected component:** unstable React Server Components (RSC) APIs in
  `react-router`. Vulnerable range: `>= 7.12.0, < 8.3.0`. Only patched
  release: `8.3.0` (verified via the GitHub Advisory Database API on
  2026-07-30), which is outside the supported range `react-router-dom ^7.6.2`
  in `web/dashboard/` — no patch exists within range, and a major-version
  upgrade to 8.x is out of scope for advisory hygiene.
- **Resolved version at decision time:** `react-router@7.18.1` (via
  `react-router-dom`), reported by `pnpm audit --prod` in `web/dashboard/`.
- **Decision:** Not applicable. The advisory only affects applications using
  the unstable RSC APIs. The dashboard is a Vite-built client-side SPA; it
  has no server-rendering or RSC code path.
- **Evidence (2026-07-30):**
  - `grep -rniE "unstable_|react-server|/rsc|createFromReadableStream|RSC"
    web/dashboard/src index.html vite.config.ts` returns no matches.
  - The only react-router import in `web/dashboard/src/` is the stable
    `react-router-dom` package (client-side routing only).
- **Allowlist note:** the repo has no `pnpm audit` allowlist mechanism (no
  `.pnpmfile.cjs`, no audit configuration in `package.json`, no audit step in
  CI), so this document is the suppression record; the advisory will continue
  to appear in `pnpm audit --prod` output until `react-router` is upgraded
  past 8.3.0.
- **Revalidate by:** 2026-10-28 (90 days), or immediately if the dashboard
  adopts server rendering / RSC APIs, or if a patched 7.x release appears.
