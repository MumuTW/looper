# loopernet deployment (withdrawn)

`loopernet` is not a supported deployment target in the current Looper product.
The repository retains dormant protocol and server code for design history, but
`looperd` has no safe workflow that issues, persists, rotates, recovers, and
revokes Node credentials. Running the service therefore cannot create a usable
Routed Looper installation.

Do not deploy `loopernet` or add `[network]` / `projects[].network` configuration.
Looper rejects those settings and does not expose Network status or maintenance
through its API. A future restoration must first define one crash-safe enrollment
authority, including compensation for remote-success/local-persistence failure,
bounded secret transport, and recoverable leave semantics.

ADRs 0007 through 0011 describe the earlier routing design. They are historical
records, not current setup instructions.
