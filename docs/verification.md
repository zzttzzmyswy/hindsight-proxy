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

## 2. Start the proxy against it

```bash
HINDSIGHT_TOKEN=e2e-management-token HINDSIGHT_URL=http://127.0.0.1:8888 \
  CONFIG_PATH=./tokens.json LISTEN_ADDR=127.0.0.1:8890 \
  go run ./cmd/hindsight-proxy
```

## 3. Run the checks

```bash
PROXY=http://127.0.0.1:8890 UPSTREAM=http://127.0.0.1:8888 \
UPSTREAM_TOKEN=e2e-management-token BANK_B=<TOKEN_B's bank> \
./scripts/verify_e2e.sh
```

The script writes to the banks its routing table names, so point it at a scratch
deployment.

## What the checks establish

- **Unknown token is rejected.** An unconfigured bearer token and a missing
  `Authorization` header both get 401; neither falls back to a bank.
- **Caller tokens are useless against Hindsight directly.** Each caller token
  gets 401 from port 8888, so the proxy really is the only way in.
- **Banks stay isolated.** A caller on one bank cannot recall a memory written
  by a caller on another, verified with real embeddings driving the search.
- **A forged ownership tag never reaches the store.** A write sending
  `tags:["agent:Mika","AGENT:Mika"," agent:Mika "]` lands with exactly
  `["agent:Solo","project:apollo"]`: every forgery stripped, the real tag
  injected, legitimate tags kept. Confirmed by reading the stored document, not
  just the response.
- **Hot reload works.** Adding a token to the routing file takes effect with no
  restart, and its tool list is trimmed as configured.
- **Read scope works.** An `own`-scoped caller sees its own memories and neither
  another agent's tagged memories nor untagged ones, while a `shared`-scoped
  caller on the same bank sees all of them.
- **A missing bank is created on demand.** A write routed to a bank that does
  not exist yet succeeds with no manual step.
- **The tool surface fits its budget.** 2689 characters across six tools,
  against a 3000 limit.

## 4. Clean up

```bash
docker rm -f hindsight-e2e
```
