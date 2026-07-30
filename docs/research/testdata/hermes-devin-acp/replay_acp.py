#!/usr/bin/env python3
"""Replay Hermes's copilot_acp_client handshake against an ACP server.

Mirrors the JSON-RPC sequence in Hermes v2026.7.20
agent/copilot_acp_client.py: initialize -> session/new -> session/prompt,
collecting agent_message_chunk / agent_thought_chunk updates, denying
session/request_permission, and rejecting unknown client methods with -32601.

This is the probe behind docs/research/hermes-devin-acp-spike.md. It exists so
the protocol, token, and error-code claims in that document can be re-derived
rather than taken on trust.

SCOPE: this deliberately does NOT implement the fs/read_text_file and
fs/write_text_file handlers that Hermes services, and so declares no fs client
capability. It is a protocol and permission-flow probe, not a stand-in for the
shim's filesystem behavior — a prompt that drives the backend to read or write
files will diverge from what Hermes would do. Read the shim itself for that.

SAFETY: the ACP server is both launched in, and told to use, the --cwd you
pass. Devin runs its own native tools in its own loop, and nothing here
sandboxes it, so point this at a disposable directory.

Usage:
  ./replay_acp.py --cwd /path/to/disposable/dir
  ./replay_acp.py --cwd /tmp/scratch --agent-type summarizer
  ./replay_acp.py --cwd /tmp/scratch --prompt-file custom_prompt.txt
"""
from __future__ import annotations

import argparse
import json
import os
import queue
import subprocess
import sys
import threading
import time

DEFAULT_PROMPT = (
    "Respond with exactly the text HERMES_DEVIN_ACP_OK and nothing else. "
    "Do not use any tools."
)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--cwd", required=True, help="Disposable working directory for the ACP session")
    ap.add_argument("--command", default="devin", help="ACP server executable (default: devin)")
    ap.add_argument("--model", default="glm-5-2")
    ap.add_argument("--agent-type", default=None, help="e.g. summarizer (no tools)")
    ap.add_argument("--prompt-file", default=None, help="Read the prompt from this file")
    ap.add_argument("--timeout", type=float, default=180.0)
    ap.add_argument("--show-thoughts", action="store_true")
    args = ap.parse_args()

    # Resolve before use: a relative --cwd would otherwise mean one thing to
    # Popen and another to whatever reads the session/new value.
    cwd = os.path.realpath(os.path.expanduser(args.cwd))
    if not os.path.isdir(cwd):
        print(f"--cwd is not a directory: {cwd}", file=sys.stderr)
        return 2

    cmd = [args.command, "acp"]
    if args.agent_type:
        cmd += ["--agent-type", args.agent_type]
    cmd += ["--model", args.model]

    prompt_text = DEFAULT_PROMPT
    if args.prompt_file:
        with open(args.prompt_file, encoding="utf-8") as fh:
            prompt_text = fh.read()

    proc = subprocess.Popen(
        cmd,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        encoding="utf-8",
        errors="replace",
        bufsize=1,
        # Launch in the disposable directory too, not just declare it in
        # session/new: the server's own startup, config discovery, and any
        # native process it spawns are rooted here.
        cwd=cwd,
    )

    inbox: "queue.Queue[dict]" = queue.Queue()
    stderr_lines: list[str] = []
    text_parts: list[str] = []
    thought_parts: list[str] = []
    unknown_methods: list[str] = []
    permission_requests = 0

    def stdout_reader() -> None:
        for line in proc.stdout:  # type: ignore[union-attr]
            try:
                inbox.put(json.loads(line))
            except Exception:
                inbox.put({"raw": line.rstrip("\n")})

    def stderr_reader() -> None:
        for line in proc.stderr:  # type: ignore[union-attr]
            stderr_lines.append(line.rstrip("\n"))

    threading.Thread(target=stdout_reader, daemon=True).start()
    threading.Thread(target=stderr_reader, daemon=True).start()

    def handle_server_message(msg: dict) -> bool:
        nonlocal permission_requests
        method = msg.get("method")
        if not isinstance(method, str):
            return False
        if method == "session/update":
            update = (msg.get("params") or {}).get("update") or {}
            kind = str(update.get("sessionUpdate") or "")
            content = update.get("content") or {}
            chunk = content.get("text") if isinstance(content, dict) else ""
            if kind == "agent_message_chunk" and chunk:
                text_parts.append(chunk)
            elif kind == "agent_thought_chunk" and chunk:
                thought_parts.append(chunk)
            else:
                print(f"  [update] {kind}", flush=True)
            return True
        mid = msg.get("id")
        if method == "session/request_permission":
            permission_requests += 1
            resp = {"jsonrpc": "2.0", "id": mid, "result": {"outcome": {"outcome": "cancelled"}}}
        else:
            unknown_methods.append(method)
            resp = {
                "jsonrpc": "2.0",
                "id": mid,
                "error": {"code": -32601, "message": f"'{method}' not supported by Hermes yet."},
            }
        proc.stdin.write(json.dumps(resp) + "\n")  # type: ignore[union-attr]
        proc.stdin.flush()  # type: ignore[union-attr]
        return True

    next_id = 0

    def request(method: str, params: dict):
        nonlocal next_id
        next_id += 1
        rid = next_id
        proc.stdin.write(  # type: ignore[union-attr]
            json.dumps({"jsonrpc": "2.0", "id": rid, "method": method, "params": params}) + "\n"
        )
        proc.stdin.flush()  # type: ignore[union-attr]
        deadline = time.monotonic() + args.timeout
        while time.monotonic() < deadline:
            if proc.poll() is not None:
                break
            try:
                msg = inbox.get(timeout=0.1)
            except queue.Empty:
                continue
            if handle_server_message(msg):
                continue
            if msg.get("id") != rid:
                print(f"  [stray] {json.dumps(msg)[:200]}", flush=True)
                continue
            if "error" in msg:
                raise RuntimeError(f"{method} failed: {msg['error']}")
            return msg.get("result")
        tail = "\n".join(stderr_lines[-15:])
        raise RuntimeError(f"{method}: process exited or timed out. stderr tail:\n{tail}")

    try:
        init = request(
            "initialize",
            {
                "protocolVersion": 1,
                # No fs capability: this probe does not implement those
                # handlers, and advertising them would invite requests it
                # would then wrongly answer with -32601 (see SCOPE above).
                "clientCapabilities": {},
                "clientInfo": {"name": "hermes-agent", "title": "Hermes Agent", "version": "0.0.0"},
            },
        )
        print("initialize OK:", json.dumps(init, ensure_ascii=False)[:400])

        session = request("session/new", {"cwd": cwd, "mcpServers": []}) or {}
        sid = session.get("sessionId")
        print("session/new OK:", json.dumps(session, ensure_ascii=False)[:400])
        if not sid:
            print("FAIL: no sessionId", file=sys.stderr)
            return 1

        result = request(
            "session/prompt",
            {"sessionId": sid, "prompt": [{"type": "text", "text": prompt_text}]},
        )
        print("session/prompt result:", json.dumps(result, ensure_ascii=False)[:300])
        print("--- collected text ---")
        print("".join(text_parts))
        if args.show_thoughts:
            print("--- thoughts ---")
            print("".join(thought_parts))
        print(
            f"--- thought chars: {sum(len(t) for t in thought_parts)}"
            f" | permission reqs: {permission_requests}"
            f" | unknown client methods: {unknown_methods}"
        )

        # In default mode this probe is an evidence check, not just a demo:
        # a session/prompt that completes but returns the wrong constant, a
        # non-end_turn stop reason, or no usage data means the recorded
        # contract in hermes-devin-acp-spike.md has drifted, and the probe
        # must fail loud rather than report success. Custom-prompt mode
        # (--prompt-file) is for open-ended probing and only reports what came
        # back, so its expectations are defined by the caller, not here.
        if args.prompt_file is None:
            failures = []
            collected = "".join(text_parts).strip()
            if collected != "HERMES_DEVIN_ACP_OK":
                failures.append(f"collected text != HERMES_DEVIN_ACP_OK (got {collected!r})")
            stop = result.get("stopReason") if isinstance(result, dict) else None
            if stop != "end_turn":
                failures.append(f"stopReason != end_turn (got {stop!r})")
            usage = result.get("usage") if isinstance(result, dict) else None
            if not (isinstance(usage, dict) and usage.get("totalTokens")):
                failures.append(f"missing/empty usage.totalTokens (got {usage!r})")
            if failures:
                for f in failures:
                    print(f"FAIL: {f}", file=sys.stderr)
                return 1
        return 0
    finally:
        try:
            proc.terminate()
            proc.wait(timeout=2)
        except Exception:
            proc.kill()


if __name__ == "__main__":
    sys.exit(main())
