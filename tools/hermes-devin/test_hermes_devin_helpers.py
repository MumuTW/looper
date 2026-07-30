"""Regression coverage for the self-contained Hermes/Devin helper scripts."""

from __future__ import annotations

import importlib.util
import ast
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
from types import SimpleNamespace
import unittest


REPO_ROOT = Path(__file__).resolve().parents[2]


def load_module(name: str, path: Path):
    spec = importlib.util.spec_from_file_location(name, path)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


MEMORY_MCP = load_module("memory_mcp_server_for_test", Path(__file__).with_name("memory_mcp_server.py"))
REPLAY_ACP = load_module(
    "replay_acp_for_test",
    REPO_ROOT / "docs" / "research" / "testdata" / "hermes-devin-acp" / "replay_acp.py",
)
STOCK_ACP_CLIENT = Path(__file__).with_name("testdata") / "copilot_acp_client.v2026.7.20.py"


class HermesDevinHelperTests(unittest.TestCase):
    def test_pinned_patch_applies_to_the_checked_in_stock_fixture_and_reverts(self):
        stock = STOCK_ACP_CLIENT.read_bytes()
        self.assertEqual(
            hashlib.sha256(stock).hexdigest(),
            "03190fcd4f9c985cab5cbaa90f7391cad8122148d7d300a81da8be0c2189c4bf",
        )
        with tempfile.TemporaryDirectory() as temp:
            install = Path(temp) / "hermes-agent"
            target = install / "agent" / "copilot_acp_client.py"
            target.parent.mkdir(parents=True)
            target.write_bytes(stock)
            script = REPO_ROOT / "tools" / "hermes-devin" / "apply-hermes-patch.sh"
            env = {**os.environ, "HERMES_INSTALL_DIR": str(install)}
            applied = subprocess.run([str(script)], env=env, text=True, capture_output=True, check=False)
            self.assertEqual(applied.returncode, 0, applied.stderr)
            self.assertEqual(
                hashlib.sha256(target.read_bytes()).hexdigest(),
                "4ec44e3260a9d86bba91e19a3dd8dc1d9f793e341855df19fd876b122bac1517",
            )
            reverted = subprocess.run([str(script), "--revert"], env=env, text=True, capture_output=True, check=False)
            self.assertEqual(reverted.returncode, 0, reverted.stderr)
            self.assertEqual(target.read_bytes(), stock)

    def test_tagged_and_captured_bare_tool_calls_use_the_pinned_hermes_parser(self):
        """Execute the parser AST from the exact stock source without its app deps."""
        source = STOCK_ACP_CLIENT.read_text(encoding="utf-8")
        tree = ast.parse(source)
        namespace = {"json": json, "re": re}
        wanted_assignments = {"_TOOL_CALL_BLOCK_RE", "_TOOL_CALL_JSON_RE"}
        for node in tree.body:
            if isinstance(node, ast.Assign) and any(
                isinstance(target, ast.Name) and target.id in wanted_assignments for target in node.targets
            ):
                exec(compile(ast.Module(body=[node], type_ignores=[]), str(STOCK_ACP_CLIENT), "exec"), namespace)
        namespace["_build_openai_tool_call"] = lambda **kwargs: SimpleNamespace(
            id=kwargs["call_id"], function=SimpleNamespace(name=kwargs["name"], arguments=kwargs["arguments"])
        )
        parser = next(node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name == "_extract_tool_calls_from_text")
        exec(compile(ast.Module(body=[parser], type_ignores=[]), str(STOCK_ACP_CLIENT), "exec"), namespace)
        captured = (
            '{"id": "call_1", "type": "function", "function": '
            '{"name": "memory", "arguments": "{\\"action\\": \\"add\\", \\"content\\": \\"LOOPER-TEST-42\\"}"}}'
        )
        for payload in (captured, f"<tool_call>{captured}</tool_call>"):
            calls, residual = namespace["_extract_tool_calls_from_text"](payload)
            self.assertEqual(residual, "")
            self.assertEqual(len(calls), 1)
            self.assertEqual(calls[0].function.name, "memory")
            self.assertEqual(calls[0].function.arguments, '{"action": "add", "content": "LOOPER-TEST-42"}')

    def test_explicit_falsy_or_unknown_target_is_rejected_before_preflight(self):
        for target in ("", False, 0, None, [], {}, "users", "User"):
            text, is_error = MEMORY_MCP.call_tool("hermes_memory_read", {"target": target})
            self.assertTrue(is_error, target)
            self.assertIn("Invalid target", text)
            self.assertIn(repr(target), text)

    def test_permission_replay_defaults_to_deny_and_only_selects_allow_once(self):
        offered = {
            "options": [
                {"optionId": "allow_session"},
                {"optionId": "allow_once"},
                {"optionId": "allow_always"},
            ]
        }
        self.assertEqual(
            REPLAY_ACP.permission_outcome(offered, approve_allow_once=False),
            {"outcome": {"outcome": "cancelled"}},
        )
        self.assertEqual(
            REPLAY_ACP.permission_outcome({"options": [{"optionId": "allow_session"}]}, approve_allow_once=True),
            {"outcome": {"outcome": "cancelled"}},
        )
        self.assertEqual(
            REPLAY_ACP.permission_outcome(offered, approve_allow_once=True),
            {"outcome": {"outcome": "selected", "optionId": "allow_once"}},
        )

    def test_non_bash_source_explains_the_portable_alternative(self):
        shell = shutil.which("zsh") or shutil.which("dash")
        if shell is None:
            self.skipTest("no non-Bash POSIX shell is installed")
        result = subprocess.run(
            [shell, "-c", '. "$PROFILE_SCRIPT"'],
            env={**os.environ, "PROFILE_SCRIPT": str(REPO_ROOT / "scripts" / "hermes-profile.sh")},
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("must be sourced from Bash", result.stderr)
        self.assertIn("--print", result.stderr)
        self.assertNotIn("Bad substitution", result.stderr)

    def test_sourcing_exports_only_hermes_home_without_clobbering_caller_state(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            profile = root / "hermes-root" / "profiles" / "looper"
            profile.mkdir(parents=True)
            command = (
                'FORCE=caller-force PROFILE_CREATED=caller-created REPO_ROOT=caller-root; '
                'set -- unrelated-arg; . "$PROFILE_SCRIPT"; status=$?; '
                'test "$status" -eq 0; test "$FORCE" = caller-force; '
                'test "$PROFILE_CREATED" = caller-created; test "$REPO_ROOT" = caller-root; '
                'test "$HERMES_HOME" = "$HERMES_ROOT/profiles/looper"; '
                '! declare -F write_profile_file >/dev/null; '
                '! declare -F __looper_select_hermes_profile >/dev/null'
            )
            result = subprocess.run(
                ["bash", "-c", command],
                env={
                    **os.environ,
                    "HERMES_ROOT": str(root / "hermes-root"),
                    "PROFILE_SCRIPT": str(REPO_ROOT / "scripts" / "hermes-profile.sh"),
                },
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_fresh_bootstrap_replaces_seed_defaults_and_preserves_empty_allowlist(self):
        with tempfile.TemporaryDirectory() as temp:
            temp_root = Path(temp)
            fake_bin = temp_root / "bin"
            fake_bin.mkdir()
            fake_hermes = fake_bin / "hermes"
            fake_hermes.write_text(
                "#!/usr/bin/env bash\n"
                "set -euo pipefail\n"
                "profile=\"$3\"\n"
                "profile_dir=\"$HERMES_HOME/profiles/$profile\"\n"
                "mkdir -p \"$profile_dir\"\n"
                "printf 'stock-config\\n' > \"$profile_dir/config.yaml\"\n"
                "printf 'stock-env\\n' > \"$profile_dir/.env\"\n",
                encoding="utf-8",
            )
            fake_hermes.chmod(0o755)
            env = {
                **os.environ,
                "HOME": str(temp_root / "home"),
                "HERMES_ROOT": str(temp_root / "hermes-root"),
                "LOOPER_ALLOWED_TOOLS": "",
                "PATH": f"{fake_bin}{os.pathsep}{os.environ['PATH']}",
            }
            result = subprocess.run(
                [str(REPO_ROOT / "scripts" / "hermes-profile.sh"), "--bootstrap"],
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            profile = Path(env["HERMES_ROOT"]) / "profiles" / "looper"
            self.assertIn("provider: copilot-acp", (profile / "config.yaml").read_text(encoding="utf-8"))
            profile_env = (profile / ".env").read_text(encoding="utf-8")
            self.assertTrue(profile_env.endswith("HERMES_ACP_ALLOWED_MCP_TOOLS="))
            self.assertNotIn("mcp__hermes-memory__hermes_memory_add", profile_env)


if __name__ == "__main__":
    unittest.main()
