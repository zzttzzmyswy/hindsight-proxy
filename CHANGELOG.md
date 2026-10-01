# Changelog

## v0.1.2

Fixes the deployment path and makes the end-to-end script actually reproduce the
claims made for it. No change to routing, ownership or tool behaviour.

- Fix the compose healthcheck, which named `/hindsight-proxy` while the image
  puts the binary at `/usr/local/bin/hindsight-proxy`. An exec-form healthcheck
  does not inherit `ENTRYPOINT`, so the container reported unhealthy forever
  without ever restarting — the failure mode that is easiest to miss.
- Stop the compose file from declaring a GHCR image that was never published.
  `docker compose up -d --build` builds the tag locally instead.
- Rebuild `scripts/verify_e2e.sh` so it supports the checks it is cited for:
  it generates and reads back its own routing table (no tokens to align by
  hand), covers hot reload and read scope end to end, and no longer asserts on
  upstream behaviour that does not hold.
- Assert writes at the document layer by content hash. Hindsight derives
  memories asynchronously and rewrites their text, so the previous
  search-the-memories checks could fail on a write that had succeeded.
- Document the three readings of the tool-surface budget, including the one
  that exceeds 3000 characters (`tools/list` wire bytes, 3441) — the previous
  docs quoted only the most favourable reading without saying so.
- Fix `README.md`'s copy step (the example file is `tokens.example.json`), and
  describe `bank_id` in the request body as dropped rather than "ignored".
- Remove a duplicated `if s.healthcheck` block in `main.go`.

## v0.1.1

Docs and hardening on top of v0.1.0. No behaviour change to routing or
ownership.

- Add `docs/verification.md` and `scripts/verify_e2e.sh` for end-to-end
  verification against a real Hindsight, alongside the fake-upstream Go suite.
- Fail the process if a destructive Hindsight tool ever enters the registry,
  so the guarantee is enforced rather than only documented.
- Fix the Dockerfile Go version, which was behind `go.mod` and broke the image
  build.
- Add `VERSION` as the single release-version source, plus a `Makefile` with
  `check`, `image`, `dist` and `release` targets.
- Remove dead code.
- Add concurrency coverage: many callers across distinct banks at once, and
  calls in flight while the routing table is reloaded.

## v0.1.0

First release. A token-routed MCP proxy in front of Hindsight, replacing the
in-process `OperationValidatorExtension` plugin.

- token → bank routing from a hot-reloaded JSON table; unknown token is 401
  with no default-bank fallback.
- bank created on demand, with the confirmed set cached.
- six tools with hand-written descriptions and trimmed schemas, 2689 characters
  total; per-token trimming via a `tools` allow-list.
- ownership tag injected by the proxy; caller-supplied `agent:*` tags stripped.
- optional per-caller `read_scope: "own"`, enforced on the response as well as
  the request.
- destructive upstream tools are not on this surface at all.
