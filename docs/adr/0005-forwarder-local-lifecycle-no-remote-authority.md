# ADR 0005: Webhook forwarder lifecycle is local-only

`looperd` may persist and reconcile records for `gh webhook forward` subprocesses that it spawned. The persisted row identifies a local process by repo, PID, process start time, executable path, endpoint, events, and daemon instance.

The authority for adopting or signalling a process is the local identity gate: PID plus process start time plus executable and command shape. `looperd` does not infer process ownership from GitHub hook state.

When `gh webhook forward` fails with `Hook already exists on this repository`, `looperd` may make one narrow GitHub hooks API cleanup attempt for the same repo before latching. That cleanup may only delete hooks where `name=cli` and `config.url=https://webhook-forwarder.github.com/hook`, which is the same conservative stale-hook identity used by the manual cleanup command. If cleanup fails or no matching stale hook is found, the forwarder latches and polling remains the fallback.

An explicit user-invoked CLI cleanup command may inspect and delete GitHub CLI `cli` hooks that point at `https://webhook-forwarder.github.com/hook`. Outside the automatic single retry above, this command remains the manual remediation path. It must stay dry-run by default, require an explicit target repo, and require explicit user confirmation before deletion.

Rejected alternatives:

- broad automatic daemon deletion or adoption of remote GitHub hooks, because `gh webhook forward` owns that lifecycle and Looper should not mutate remote hook state on inference alone;
- enumerating unrelated `gh` processes on first upgrade boot, because Looper has no prior local record proving it spawned them;
- keeping forwarders alive during a graceful restart, because stop and restart are indistinguishable without a future re-exec design.

If local lifecycle reconciliation fails, webhook latency falls back to the existing polling interval. Correctness remains with the poller; this lifecycle only improves delivery latency and operator diagnostics.
