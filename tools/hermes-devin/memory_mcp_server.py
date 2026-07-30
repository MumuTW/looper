#!/usr/bin/env python3
"""Expose Hermes Agent's on-disk memory store as MCP tools over stdio.

Devin's ACP backend can call MCP tools natively but has no idea Hermes memory
exists. This server bridges the two: Devin spawns it as a subprocess, lists the
four hermes_memory_* tools, and calls them; each call goes through Hermes's own
``memory_tool`` dispatch so the char limits, write gate, and file locking all
behave exactly as they do inside a Hermes session.

This is a carried patch component for an unsupported integration — nothing in
Hermes or Devin ships it, and neither project promises the interfaces it leans
on. It is pinned to Hermes v2026.7.20 / devin 3000.3.22; re-verify against
tools/memory_tool.py after upgrading either one.

Protocol is hand-rolled JSON-RPC (no `mcp` SDK): the Hermes install is the
interpreter environment here and it does not carry that dependency.

Usage:
  HERMES_HOME=~/.hermes/profiles/looper ./memory_mcp_server.py
  HERMES_HOME=~/.hermes/profiles/looper ./memory_mcp_server.py --selftest
"""
from __future__ import annotations

import json
import os
import sys
import traceback
from pathlib import Path

PROTOCOL_VERSION = "2024-11-05"
SERVER_NAME = "hermes-memory"
SERVER_VERSION = "0.1.0"

DEFAULT_INSTALL_DIR = Path.home() / ".hermes" / "hermes-agent"

# Populated by _import_hermes(). A failed import is recorded rather than raised:
# dying at startup surfaces to the user as an inscrutable "server not found"
# from Devin, whereas a live server can explain itself in a tool result.
_load_on_disk_store = None
_memory_tool = None
_IMPORT_ERROR: str | None = None

_TARGET_SCHEMA = {
    "type": "string",
    "enum": ["memory", "user"],
    "description": (
        "Which store to act on. 'memory' (default) is the agent's own durable notes "
        "about the project and the work. 'user' is the profile of who the user is — "
        "their identity, role, and standing preferences."
    ),
}

TOOLS = [
    {
        "name": "hermes_memory_add",
        "description": (
            "Persist a durable note in Hermes's memory so it survives this session and "
            "is available to every future one. This is the ONLY way to remember something "
            "past the end of the conversation — ordinary replies are forgotten. Use it for "
            "facts with a shelf life: project conventions, decisions and their rationale, "
            "environment quirks, and stated user preferences. Do not use it for transient "
            "state (what you are doing right now, contents of a file you can re-read). "
            "Write one self-contained fact per call, phrased so it is understandable with "
            "no conversation context. The store is char-limited; an add that would overflow "
            "it comes back as an error asking you to consolidate existing entries first."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "content": {
                    "type": "string",
                    "description": "The note to store: one self-contained fact, no conversational framing.",
                },
                "target": _TARGET_SCHEMA,
            },
            "required": ["content"],
        },
    },
    {
        "name": "hermes_memory_replace",
        "description": (
            "Rewrite an existing memory entry in place. Use when a stored fact is now wrong "
            "or out of date — correcting it is better than adding a second, contradictory "
            "entry, and better than remove-then-add because it stays within the char budget. "
            "old_text must be a substring that uniquely identifies the entry to rewrite; call "
            "hermes_memory_read first if you are unsure of the exact wording."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "old_text": {
                    "type": "string",
                    "description": "A short substring uniquely identifying the entry to rewrite.",
                },
                "content": {
                    "type": "string",
                    "description": "The full replacement text for that entry.",
                },
                "target": _TARGET_SCHEMA,
            },
            "required": ["old_text", "content"],
        },
    },
    {
        "name": "hermes_memory_remove",
        "description": (
            "Delete a memory entry permanently. Use when a stored fact has become obsolete "
            "rather than merely wrong (the feature was removed, the preference was retracted) "
            "or when freeing space to stay under the char limit. If the fact still applies in "
            "an amended form, prefer hermes_memory_replace. old_text must be a substring that "
            "uniquely identifies the entry."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "old_text": {
                    "type": "string",
                    "description": "A short substring uniquely identifying the entry to delete.",
                },
                "target": _TARGET_SCHEMA,
            },
            "required": ["old_text"],
        },
    },
    {
        "name": "hermes_memory_read",
        "description": (
            "List everything currently stored in Hermes memory, plus the char budget used. "
            "Call it early when prior context about this user or project would change what "
            "you do, and before any replace/remove so you can quote an entry's exact wording "
            "as old_text."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {"target": _TARGET_SCHEMA},
            "required": [],
        },
    },
]

TOOL_NAMES = {t["name"] for t in TOOLS}
VALID_TARGETS = {"memory", "user"}


def log(msg: str) -> None:
    """Diagnostics go to stderr only — stdout carries framed JSON-RPC and nothing else."""
    print(msg, file=sys.stderr, flush=True)


def _reexec_into_hermes_venv() -> None:
    """Re-exec under the Hermes venv interpreter when started with another one.

    Hermes ships its own venv and imports third-party packages (yaml) on the
    memory import path, so a bare ``python3`` cannot load it. Devin's MCP config
    only names a command, and whoever writes that config should not have to know
    the venv path — so find it ourselves. The env guard makes this a one-shot:
    if the venv interpreter still can't import Hermes we fall through to the
    normal degraded-server behaviour instead of exec-looping.
    """
    if os.environ.get("HERMES_MCP_REEXEC"):
        return
    install_dir = Path(os.environ.get("HERMES_INSTALL_DIR") or DEFAULT_INSTALL_DIR).expanduser()
    for candidate in (install_dir / "venv" / "bin" / "python", install_dir / "venv" / "Scripts" / "python.exe"):
        if not candidate.exists() or str(candidate) == sys.executable:
            continue
        env = dict(os.environ, HERMES_MCP_REEXEC="1")
        # PYTHONPATH/PYTHONHOME inherited from the spawning agent would defeat
        # the venv; the `hermes` launcher script unsets them for the same reason.
        env.pop("PYTHONPATH", None)
        env.pop("PYTHONHOME", None)
        try:
            os.execve(str(candidate), [str(candidate), os.path.abspath(__file__), *sys.argv[1:]], env)
        except OSError as exc:
            log(f"could not re-exec into {candidate}: {exc!r}")
        return


def _import_hermes() -> None:
    """Put the Hermes install on sys.path and bind its memory entry points."""
    global _load_on_disk_store, _memory_tool, _IMPORT_ERROR

    install_dir = Path(os.environ.get("HERMES_INSTALL_DIR") or DEFAULT_INSTALL_DIR).expanduser()
    try:
        if str(install_dir) not in sys.path:
            sys.path.insert(0, str(install_dir))
        from tools.memory_tool import load_on_disk_store, memory_tool

        _load_on_disk_store = load_on_disk_store
        _memory_tool = memory_tool
    except Exception as exc:
        _IMPORT_ERROR = (
            f"Could not import Hermes memory from {install_dir}: {exc!r}. "
            "Set HERMES_INSTALL_DIR to the hermes-agent package root."
        )
        log(_IMPORT_ERROR)


def _preflight() -> str | None:
    """Return an error string when memory cannot be touched at all, else None."""
    if _IMPORT_ERROR:
        return _IMPORT_ERROR
    # Hermes falls back to the platform-default profile when HERMES_HOME is
    # unset. For a Devin-spawned subprocess that silently writes another
    # profile's memory, so refuse instead of guessing.
    if not (os.environ.get("HERMES_HOME") or "").strip():
        return (
            "HERMES_HOME is not set in this server's environment, so the Hermes profile "
            "to write cannot be determined. Refusing rather than writing to the default "
            "profile. Set HERMES_HOME in the MCP server config for this session."
        )
    return None


def _read_entries(store, target: str) -> str:
    """Render the current entries as the JSON shape memory_tool uses for errors."""
    entries = store.user_entries if target == "user" else store.memory_entries
    limit = store.user_char_limit if target == "user" else store.memory_char_limit
    used = sum(len(e) for e in entries)
    return json.dumps(
        {
            "success": True,
            "target": target,
            "current_entries": list(entries),
            "usage": f"{used:,}/{limit:,}",
        },
        ensure_ascii=False,
    )


def call_tool(name: str, args: dict) -> tuple[str, bool]:
    """Run one hermes_memory_* tool. Returns (text, is_error); never raises."""
    if name not in TOOL_NAMES:
        return json.dumps({"success": False, "error": f"Unknown tool '{name}'."}), True

    # Default only when the selector is absent. `or "memory"` would turn an
    # explicitly supplied but falsy invalid selector ("", false, 0, null)
    # into a real project-memory mutation.
    target = args["target"] if "target" in args else "memory"
    if not isinstance(target, str) or target not in VALID_TARGETS:
        return json.dumps(
            {
                "success": False,
                "error": f"Invalid target {target!r}. Must be one of: " + ", ".join(sorted(VALID_TARGETS)),
            },
            ensure_ascii=False,
        ), True

    problem = _preflight()
    if problem:
        return json.dumps({"success": False, "error": problem}, ensure_ascii=False), True

    try:
        # Rebuilt per call: Hermes's own sessions mutate the same files under a
        # file lock, so a cached store would hand back stale entries.
        store = _load_on_disk_store()
        if name == "hermes_memory_read":
            text = _read_entries(store, target)
        elif name == "hermes_memory_add":
            text = _memory_tool(action="add", target=target, content=args.get("content"), store=store)
        elif name == "hermes_memory_replace":
            text = _memory_tool(
                action="replace",
                target=target,
                old_text=args.get("old_text"),
                content=args.get("content"),
                store=store,
            )
        else:
            text = _memory_tool(action="remove", target=target, old_text=args.get("old_text"), store=store)
    except Exception as exc:
        log(traceback.format_exc())
        return json.dumps({"success": False, "error": f"{name} failed: {exc!r}"}, ensure_ascii=False), True

    if not isinstance(text, str):
        text = json.dumps(text, ensure_ascii=False, default=str)

    # memory_tool reports failure inside its JSON payload, not by raising; mirror
    # that into the MCP isError flag. A non-JSON body is passed through as-is.
    is_error = False
    try:
        parsed = json.loads(text)
        if isinstance(parsed, dict):
            is_error = parsed.get("success") is False or "error" in parsed
    except (ValueError, TypeError):
        pass
    return text, is_error


def handle(msg: dict) -> dict | None:
    """Dispatch one JSON-RPC message. Returns None for notifications."""
    method = msg.get("method")
    mid = msg.get("id")
    is_notification = mid is None

    if method == "initialize":
        result = {
            "protocolVersion": PROTOCOL_VERSION,
            "capabilities": {"tools": {}},
            "serverInfo": {"name": SERVER_NAME, "version": SERVER_VERSION},
        }
    elif method in ("notifications/initialized", "initialized"):
        return None
    elif method == "tools/list":
        result = {"tools": TOOLS}
    elif method == "tools/call":
        params = msg.get("params") or {}
        args = params.get("arguments") or {}
        text, is_error = call_tool(params.get("name"), args if isinstance(args, dict) else {})
        result = {"content": [{"type": "text", "text": text}], "isError": is_error}
    else:
        if is_notification:
            return None
        return {
            "jsonrpc": "2.0",
            "id": mid,
            "error": {"code": -32601, "message": f"Method not found: {method}"},
        }

    if is_notification:
        return None
    return {"jsonrpc": "2.0", "id": mid, "result": result}


def serve() -> int:
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
            if not isinstance(msg, dict):
                raise ValueError("JSON-RPC message must be an object")
        except Exception as exc:
            # No id to answer to, and a parse error must not end the session.
            log(f"skipping malformed stdin line: {exc!r}")
            continue
        try:
            response = handle(msg)
        except Exception:
            log(traceback.format_exc())
            mid = msg.get("id")
            if mid is None:
                continue
            response = {
                "jsonrpc": "2.0",
                "id": mid,
                "error": {"code": -32603, "message": "Internal error"},
            }
        if response is not None:
            sys.stdout.write(json.dumps(response, ensure_ascii=False) + "\n")
            sys.stdout.flush()
    return 0


def selftest() -> int:
    """Exercise tools/list and a read without an MCP client. Read-only."""
    ok = True

    log(f"HERMES_HOME={os.environ.get('HERMES_HOME') or '(unset)'}")
    log(f"import error: {_IMPORT_ERROR or 'none'}")

    listed = handle({"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
    names = [t["name"] for t in (listed or {}).get("result", {}).get("tools", [])]
    log(f"tools/list -> {names}")
    expected = ["hermes_memory_add", "hermes_memory_replace", "hermes_memory_remove", "hermes_memory_read"]
    if sorted(names) != sorted(expected):
        log(f"FAIL: tools/list mismatch, expected {expected}")
        ok = False

    notif = handle({"jsonrpc": "2.0", "method": "notifications/initialized"})
    if notif is not None:
        log("FAIL: notification produced a reply")
        ok = False

    unknown = handle({"jsonrpc": "2.0", "id": 2, "method": "no/such/method"})
    if not (unknown or {}).get("error"):
        log("FAIL: unknown method did not produce a JSON-RPC error")
        ok = False

    for target in ("memory", "user"):
        resp = handle(
            {
                "jsonrpc": "2.0",
                "id": 3,
                "method": "tools/call",
                "params": {"name": "hermes_memory_read", "arguments": {"target": target}},
            }
        )
        result = (resp or {}).get("result") or {}
        text = (result.get("content") or [{}])[0].get("text", "")
        if result.get("isError"):
            log(f"FAIL: read({target}) -> {text}")
            ok = False
            continue
        payload = json.loads(text)
        entries = payload.get("current_entries", [])
        log(f"read({target}) OK: {len(entries)} entries, usage {payload.get('usage')}")

    log("SELFTEST PASS" if ok else "SELFTEST FAIL")
    return 0 if ok else 1


def main() -> int:
    _reexec_into_hermes_venv()
    _import_hermes()
    if "--selftest" in sys.argv[1:]:
        return selftest()
    return serve()


if __name__ == "__main__":
    sys.exit(main())
