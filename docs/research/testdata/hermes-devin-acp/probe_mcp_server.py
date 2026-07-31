#!/usr/bin/env python3
"""Minimal stdio MCP server exposing one tool, used to prove that Hermes-side
tools ARE reachable through Devin's ACP backend when routed via MCP rather than
via the shim's prompt-level <tool_call> contract.

Register it with `devin mcp add <name> -- python3 probe_mcp_server.py` from the
directory the ACP session will use as cwd, then run replay_acp.py with a prompt
that asks for the tool. Set MCP_PROBE_LOG to choose where call evidence lands.

See docs/research/hermes-devin-acp-spike.md."""
import json
import sys

import os

LOG = os.environ.get("MCP_PROBE_LOG", "mcp_probe_hits.log")

TOOLS = [{
    "name": "looper_memory_store",
    "description": "Store a durable note in Looper's persistent memory. Use whenever the user asks to remember something.",
    "inputSchema": {
        "type": "object",
        "properties": {"content": {"type": "string", "description": "The note to store"}},
        "required": ["content"],
    },
}]


def reply(mid, result):
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": mid, "result": result}) + "\n")
    sys.stdout.flush()


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        msg = json.loads(line)
    except Exception:
        continue
    method = msg.get("method")
    mid = msg.get("id")
    with open(LOG, "a") as fh:
        fh.write(f"RECV {method}\n")
    if method == "initialize":
        reply(mid, {
            "protocolVersion": "2024-11-05",
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "looper-memory", "version": "0.1.0"},
        })
    elif method == "tools/list":
        reply(mid, {"tools": TOOLS})
    elif method == "tools/call":
        params = msg.get("params") or {}
        content = (params.get("arguments") or {}).get("content", "")
        with open(LOG, "a") as fh:
            fh.write(f"TOOL_CALL content={content!r}\n")
        reply(mid, {"content": [{"type": "text", "text": f"Stored: {content}"}]})
    elif mid is not None:
        reply(mid, {})
