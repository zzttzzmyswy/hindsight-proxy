# Local end-to-end verification

The Go test suite runs against a fake upstream: fast, and suitable for CI, but it
never exercises real embeddings, LLM fact extraction, or upstream tag semantics.
Do both. This is how the delivered build was verified.

## 1. Start a real Hindsight

Version 0.10.2, matching the NAS deployment. It needs an LLM for fact
extraction; any OpenAI-compatible `/v1/chat/completions` endpoint works.

```bash
docker run -d --name hindsight-e2e --network host \
  -e HINDSIGHT_API_LLM_PROVIDER=openai \
  -e HINDSIGHT_API_LLM_BASE_URL=http://127.0.0.1:8088/v1 \
  -e HINDSIGHT_API_LLM_API_KEY=<key> \
  -e HINDSIGHT_API_LLM_MODEL=claude-haiku-4-5-20251001 \
  -e HINDSIGHT_API_TENANT_EXTENSION=hindsight_api.extensions.builtin.tenant:ApiKeyTenantExtension \
  -e HINDSIGHT_API_TENANT_API_KEY=e2e-management-token \
  ghcr.io/vectorize-io/hindsight:0.10.2

until curl -sf localhost:8888/version >/dev/null; do sleep 2; done
```

Confirm auth is on, which is what makes the proxy the only entry point:

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:8888/v1/default/banks
# 401
```

## 2. Generate a routing table and start the proxy

The script owns its routing table, so there is nothing to keep in sync by hand.
It generates freshly-random tokens and reads them back itself at check time.

```bash
./scripts/verify_e2e.sh --write-config      # writes ./e2e-tokens.json

CONFIG_PATH=./e2e-tokens.json HINDSIGHT_URL=http://127.0.0.1:8888 \
  HINDSIGHT_TOKEN=e2e-management-token LISTEN_ADDR=127.0.0.1:8890 \
  go run ./cmd/hindsight-proxy
```

## 3. Run the checks

```bash
UPSTREAM_TOKEN=e2e-management-token ./scripts/verify_e2e.sh
```

The script writes to the banks that table names, so point it at a scratch
deployment.

## What the checks establish

- **Unknown token is rejected.** An unconfigured bearer token and a missing
  `Authorization` header both get 401; neither falls back to a bank.
- **Caller tokens are useless against Hindsight directly.** Each caller token
  gets 401 from port 8888, so the proxy really is the only way in.
- **Banks stay isolated.** A caller on one bank cannot recall a memory written
  by a caller on another, verified with real embeddings driving the search, and
  each write is confirmed present in its own bank at the storage layer.
- **A forged ownership tag never reaches the store.** A write sending
  `tags:["agent:E2eAlpha","AGENT:E2eAlpha"," agent:E2eAlpha ",...]` lands with
  exactly the caller's real tag plus `project:e2e`: every forgery stripped, the
  real tag injected, legitimate tags kept. Confirmed by reading the stored
  document, not the proxy's response.
- **Hot reload works.** A token added to the routing file is live with no
  restart and its tool list is trimmed as configured; an edit to an existing
  rule replaces it with no restart; and the tokens already in the table keep
  working across the reload.
- **Read scope works.** On one bank holding two writers, an `own`-scoped caller
  sees its own memories and never another writer's, while a `shared`-scoped
  caller on the same bank sees everything.
- **A missing bank is created on demand.** The bank is confirmed absent, a write
  routed to it succeeds, and the bank is confirmed present afterwards.
- **The tool surface fits its budget.** See the three readings below.

### Two things to know when reproducing this

**Hindsight derives memories asynchronously, and rewrites the text.** A retained
document is turned into memory units by an LLM, with a latency of ten-odd
seconds (the document's `memory_unit_count` goes from 0 to positive), and the
derived text is not the text that was submitted. Three consequences:

- The checks that need memories rather than documents wait for extraction
  instead of racing it.
- Storage-layer assertions avoid a text search entirely: they key on the
  document's `content_hash` (sha256 of the submitted text), because searching
  memories for a literal probe string fails even when the write succeeded.
- The `own`-scope check waits on the storage layer with the management token
  and reads through the proxy exactly once, after the data has settled. A wait
  implemented as a burst of reads through the proxy would make the result
  depend on the timing of those reads. (The failure seen in the field was an
  own-scoped recall coming back empty; our controlled comparison of
  poll-vs-wait did not reproduce a tag-ownership effect, so the storage-side
  wait is adopted because it is correct regardless of the mechanism. See the
  note in `README.md`.)

**The tool-surface budget has three readings.** They differ because MCP safety
annotations are added by the SDK at serialization time:

| Reading | Characters | Against 3000 |
| --- | --- | --- |
| description + schema (the issue's wording) | 2626 | under |
| + tool names (`ToolSurfaceSize()`, what `make check` enforces) | 2689 | under |
| actual `tools/list` bytes on the wire | 3441 | over |

The annotations account for 421 of the difference. The first two readings pass;
the third does not. The budget's purpose in the issue was to bound context
growth, which argues for the wire reading, so treat this as an open item rather
than a closed one -- `recall` and `reflect` schemas (~600 and ~500 characters)
are where the remaining fat is.

These figures use compact JSON separators (`json.Marshal` defaults, which is what
the proxy emits). Measuring the same payload with a pretty-printer's spaced
separators gives 2801 / 2864 instead; the payload is identical, so quote the
serializer basis alongside the number. Both readings agree the wire bytes exceed
3000.

## 4. Re-running, then clean up

The script is safe to run repeatedly against the same deployment: the rule it
edits belongs to a token no other check reads, and the one assertion that needs a
never-used bank reports a skip instead of a failure once that bank exists. To
re-assert absence, regenerate the table with `--write-config`.

```bash
docker rm -f hindsight-e2e
rm -f e2e-tokens.json
```
