# Changelog

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
