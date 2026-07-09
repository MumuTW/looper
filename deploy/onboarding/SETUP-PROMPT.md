# Prompt — set up and start looper (Feishu HITL)

Send the whole block below to your coding agent (Claude Code / codex / …) and run it **inside this unzipped directory**.

---

You are helping me set up and start **looper** — a daemon that runs an autonomous dev agent — together with its human-in-the-loop (HITL) Feishu integration. Work **interactively**: confirm with me before any outward-facing (GitHub / Feishu) or irreversible action.

## What's in this bundle
- `config.hitl.example.json` — the config template, with placeholders.
- `hitl.env` — the team's **shared** Feishu / Worker secrets (already filled; **do not print it, do not commit it to git, do not paste it into chat**).
- `GUIDE-hitl-setup.md` — reference doc; read it first.

## Do this
1. **Check prerequisites** and tell me what's missing:
   - `looperd` and `looper` are on PATH (if not, ask me for the path / how to install or build them).
   - My coding-agent binary (`codex` or `claude`) is installed and **logged in / authorized** (run it once to confirm it works).
   - The GitHub repo I want looper to work on is already cloned locally.
2. **Collect my settings** (ask me, don't guess):
   - GitHub repo (`owner/repo`) plus the **absolute path** of its local clone.
   - My Feishu **group chat id** (`oc_...`).
   - **You don't need to hunt for my own open_id** — step 5 grabs it automatically via `looper login` (a quick browser authorization; it writes it into `owner` and `mentionOpenIds` in the config).
   - The `productOwner` and `qa` open_ids (**team-wide**, the distributor gives you these — the product owner's / QA's `ou_...`; the template leaves `REPLACE_...` placeholders for them).
   - The absolute path of my coding-agent binary, and whether I use `codex` or `opencode` (see "Backend" below).
   - Where looper keeps its data/logs (default `~/.looper`).
3. **Write my config**: copy `config.hitl.example.json` to `~/.looper/config.json` and replace every `REPLACE_...` / `/ABSOLUTE/...` / `OWNER/REPO` / `FILLED_BY_looper_login_...` placeholder with the values above (leave the `owner` one for now — `looper login` fills it in the next step). When done, **show me the final config** to confirm (it holds no secrets, so it's safe to display).
   - **Backend**: the template defaults to `codex` (`-c model=gpt-5.4 …`). If I use `opencode`, change `agent.vendor` to `opencode`, point `command` at the opencode binary, set `model` to `openai/gpt-5.4`, and replace `args` with `["--pure","--dangerously-skip-permissions"]` (codex's `-c/-s` flags mean different things in opencode — don't mix them up).
4. **Load the shared secrets**: `source` the `hitl.env` in this bundle. **Do not** print the secret values, and **do not** copy them into the config file (the config only references variable names).
5. **Log into Feishu to get my open_id** (automatic; `source hitl.env` first so the app credentials are in the shell):
   ```sh
   source <bundle-path>/hitl.env                  # app_id/secret etc. enter this shell
   looper login --config ~/.looper/config.json    # opens a browser authorization; auto-writes my open_id into owner + mentionOpenIds
   ```
   (Prerequisite: the shared Feishu app already has `http://127.0.0.1:53682/callback` in its "redirect URL allowlist" — that's a **one-time, app-level** setup the distributor does, not your concern; if you hit `20029 redirect URL is invalid`, ask the distributor to add it.)
6. **Install it as a resident service and start it** (so it auto-restarts on sleep/wake, reboot, or crash — don't run it bare with `nohup`):
   ```sh
   looper daemon install
   looper daemon start --config ~/.looper/config.json --daemon-restart-policy on-failure
   ```
   Confirm it's alive: `looper daemon status --config ~/.looper/config.json`. (The secrets come from the `hitl.env` you sourced above and get baked into the launchd service, so they survive restarts.)
7. **Smoke test** (ask me first): create a small issue with the `looper:plan` label in my repo and confirm looper picks it up and — when there's ambiguity — posts a decision card to my Feishu group that @-mentions me.

## Guardrails
- Never commit `hitl.env` or the config into git; never paste secrets into chat.
- **Ask me first** before creating a GitHub issue/PR or posting anything to Feishu.
- If a prerequisite is missing, stop and tell me — don't pretend it's set up.
