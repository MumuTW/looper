# Pinned upstream fixture

`copilot_acp_client.v2026.7.20.py` is a byte-exact copy of
`agent/copilot_acp_client.py` from Hermes Agent v0.19.0 (upstream tag
v2026.7.20), MIT licensed, Copyright (c) 2025 Nous Research. See the upstream
LICENSE at https://github.com/NousResearch/hermes-agent.

It is vendored so `test_hermes_devin_helpers.py` can verify that the carried
patch in `../acp-permission-allowlist.patch` applies and reverts cleanly
without needing a Hermes install. Its sha256 is one of the two constants
`../apply-hermes-patch.sh` pins, so the fixture and the script drift together
or not at all.

**It must stay byte-exact.** A reconstructed or hand-edited approximation
would let the patch verify against a file that no real install has, which is
exactly the failure this fixture exists to catch. Refresh it only by copying
from a real install at the pinned version.
