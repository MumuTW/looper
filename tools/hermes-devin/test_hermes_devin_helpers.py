"""Regression coverage for the self-contained Hermes/Devin helper scripts."""

from __future__ import annotations

import importlib.util
import os
from pathlib import Path
import shutil
import subprocess
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


class HermesDevinHelperTests(unittest.TestCase):
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


if __name__ == "__main__":
    unittest.main()
