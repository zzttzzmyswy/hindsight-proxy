# Changelog

## v0.1.5

Namespace `document_id` under the writing agent, so two callers sharing a bank
can no longer overwrite each other's documents. No change to routing, tag
stripping/injection, read scope, bank auto-creation, or the tool surface.

- `retain` now prefixes each item's `document_id` with `<agent>/`, the agent
  name from the caller's routing rule. Reusing a key replaces your own document
  and nobody else's: with a shared bank, agent A writing `env:port` and agent B
  writing `env:port` produce `A/env:port` and `B/env:port`, where before the
  second write replaced the first.
- The prefix is applied whenever the rule names an agent, without checking
  whether the bank is currently shared. A bank can gain a second caller while
  the documents already written under it stay where they are, so a rule that
  switched on "is this bank shared?" would rename the ids of everything written
  before the change and strand them. One rule for every agent stays stable.
- Idempotent: an id already carrying this caller's own prefix is used as-is, so
  a `document_id` read back from recall can be written again to correct that
  same document. An id carrying someone else's prefix gets this caller's prefix
  on top (`A/B/x`), which is what keeps it out of `B`'s documents.
- A rule with no `agent` forwards `document_id` unchanged, as in v0.1.4, and a
  caller that sends no `document_id` still sends no such field.
- Config validation gains two rules, both failing closed: an agent name may not
  contain `/` (or agent `X` writing `Y/z` and agent `X/Y` writing `z` would
  resolve to the same document), and a bank named by two or more rules must name
  an agent on every one of them (nothing else namespaces the id). Startup
  refuses the table; a hot reload keeps the previous one, as before.
- Responses are unchanged: a `document_id` returned by recall or the documents
  list comes back with its prefix, un-stripped, so it round-trips.
- **Existing documents are not migrated.** A document written before this
  release under a bare id is not reachable by writing that id again: the write
  now lands on `<agent>/<id>` as a new document and the old one stays as it was.
  Re-write the fact once under the prefixed id to move it. Nothing is deleted or
  rewritten by the upgrade itself.
- Tool surface: unchanged at 2799 (`ToolSurfaceSize()`); the parameter was
  already advertised in v0.1.4 and nothing on the surface changed.
- End-to-end script: new `document_id namespace` section, driving two agents on
  one bank with the same key. One agent name carries Chinese and parentheses, so
  the check also establishes that upstream accepts a namespaced id of that
  shape. Run against a live Hindsight v0.10.2.

## v0.1.4

Make the tool surface self-documenting: an agent now learns when to recall and
retain, and can correct a fact instead of accumulating contradictions. No change
to routing, tag stripping/injection, read scope, or bank auto-creation.

- The MCP handshake now carries the memory usage protocol in `instructions`,
  appended to the existing per-caller routing text. None of the agents behind
  this proxy has Hindsight guidance in its own instruction set, so the tools
  were reachable but nothing prompted their use — a bank stayed at zero
  memories until it was written to by hand. One place to maintain, effective
  for every caller already pointed at the proxy, and `instructions` does not
  count against the tool-surface budget.
- `retain` advertises `document_id` on each item. The handler already forwarded
  it and upstream already upserts on it, but an agent could not discover the
  parameter, so a superseded fact could only pile up beside its replacement.
  This is the only correction path by design: no delete tool is exposed.
- A caller that omits `document_id` still sends no such field (`omitempty`), so
  untagged writes are not collapsed onto one document.
- Tool surface: 2799 characters (+110) on the `ToolSurfaceSize()` reading,
  3551 (+110) on the wire-byte reading. The usage protocol in `instructions`
  is not part of either figure.
- End-to-end script: new `document_id upsert` section, and a handshake check
  that `initialize` carries the usage protocol. Both were run against a live
  Hindsight v0.10.2.

## v0.1.3

Make the end-to-end script reproducible and re-runnable. No change to routing,
ownership or tool behaviour.

- Drive the `own`-scope check from a storage-layer wait rather than a burst of
  reads through the own-scoped token. A read issued while extraction is still in
  flight makes the outcome depend on timing, which a check asserting isolation
  must not do. The read path is now observed once, after the data has settled.
- Report a skip rather than a failure when the auto-create bank already exists,
  so running the script twice against one deployment is meaningful.
- Give the hot-reload check its own rule to edit, so it can no longer leave the
  tool-trimming check broken for a later run.
- Reconcile the tool-surface figures with their serializer basis: 2626 / 2689
  with compact separators (what the proxy emits), 2801 / 2864 with a
  pretty-printer's spacing. Same payload; the wire reading exceeds 3000 either
  way.
- Note the async-extraction latency explicitly, and that a literal search over
  memories fails even for a write that succeeded.

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
