# Pinned upstream fixture

`copilot_acp_client.v2026.7.20.py` is a byte-exact copy of
`agent/copilot_acp_client.py` from Hermes Agent v0.19.0. Its source is the
immutable commit
[`3ef6bbd201263d354fd83ec55b3c306ded2eb72a`](https://github.com/NousResearch/hermes-agent/commit/3ef6bbd201263d354fd83ec55b3c306ded2eb72a),
which is the commit resolved by upstream tag `v2026.7.20` when this fixture
was pinned. The separately verifiable SHA-256 of that exact upstream blob is
`eb5b4bf7bf2c4ff7deb0f2928a2fb4ada0e8584996603b88541e90d3c5e8f178`.

The copied source is MIT licensed, Copyright (c) 2025 Nous Research. The
applicable permission notice is included locally in `UPSTREAM-MIT-LICENSE`.

It is vendored so `test_hermes_devin_helpers.py` can verify that the carried
patch in `../acp-permission-allowlist.patch` applies and reverts cleanly
without needing a Hermes install. The fixture's SHA-256 and the patched result
are both asserted by the test and by `../apply-hermes-patch.sh`.

**It must stay byte-exact.** A reconstructed or hand-edited approximation
would let the patch verify against a file that no real install has, which is
exactly the failure this fixture exists to catch. Refresh it only from a
specific upstream commit or release artifact, record its independently
verifiable digest here, and regenerate the patch checksums after verifying
that the carried patch still has the same narrow `allow_once` semantics.
