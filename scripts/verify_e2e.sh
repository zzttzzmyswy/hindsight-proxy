#!/usr/bin/env bash
# End-to-end acceptance check against a LIVE Hindsight + hindsight-proxy stack.
#
# The Go test suite (go test ./...) covers all six acceptance criteria against a
# fake upstream and is the check CI runs. This script is the complementary one:
# it exercises a real deployment, where embeddings, LLM fact extraction and the
# real tag/scope semantics are involved. Criteria 2, 3, 5 and 6 are only
# established end to end here.
#
# This script OWNS its routing table. It writes ./e2e-tokens.json with freshly
# generated tokens, and you point the proxy at that same file, so there is
# nothing to keep in sync by hand:
#
#   1. ./scripts/verify_e2e.sh --write-config      # generates ./e2e-tokens.json
#   2. CONFIG_PATH=./e2e-tokens.json HINDSIGHT_URL=... HINDSIGHT_TOKEN=... \
#        ./bin/hindsight-proxy &
#   3. UPSTREAM_TOKEN=<management token> ./scripts/verify_e2e.sh
#
# It writes to the banks that table names, so point it at a scratch deployment
# rather than production.

set -uo pipefail

PROXY="${PROXY:-http://127.0.0.1:8890}"
UPSTREAM="${UPSTREAM:-http://127.0.0.1:8888}"
ROUTING_FILE="${ROUTING_FILE:-./e2e-tokens.json}"
RELOAD_WAIT="${RELOAD_WAIT:-20}"

BANK_A="ops-e2e"
BANK_B="shared-e2e"
# Target bank for the hot-reload probe added at check time. Fixed, so a re-run
# against an existing deployment is reproducible.
BANK_HOT="hot-e2e"

# Probe texts are written out in full and hashed as-is, so the storage-layer
# check can find the document by content hash.
MARKER_ALPHA="E2E marker alpha: the ops deploy key rotates on Friday."
MARKER_BETA="E2E marker beta: the shared calendar is public."
MARKER_FORGE="E2E forged-tag probe: the staging password is rotated monthly."
MARKER_OWN="E2E own-scope probe: a private note about the quarterly review."
MARKER_SCOPE_B="E2E shared-scope probe: a public note about the quarterly review."

# --write-config runs before the upstream token is needed, so the table can be
# generated (and the proxy started) without one. Bank ids that must be absent at
# start-up are pinned in the file rather than randomised here, because the
# writing process and the checking process are separate invocations and RANDOM
# would not survive between them.
if [ "${1:-}" = "--write-config" ]; then
  cat >"$ROUTING_FILE" <<EOF
{
  "default_bank": "$BANK_B",
  "tokens": {
    "$(openssl rand -hex 24)": { "agent": "E2eAlpha", "bank": "$BANK_A",         "tools": "*" },
    "$(openssl rand -hex 24)": { "agent": "E2eBeta",  "bank": "$BANK_B",         "tools": "*" },
    "$(openssl rand -hex 24)": { "agent": "E2eFresh", "bank": "fresh-e2e-$RANDOM", "tools": "*" },
    "$(openssl rand -hex 24)": { "agent": "E2eOwn",   "bank": "$BANK_B",         "tools": "*", "read_scope": "own" },
    "$(openssl rand -hex 24)": { "agent": "E2eTrim",  "bank": "$BANK_B",         "tools": ["recall", "list_tags"] }
  }
}
EOF
  echo "wrote $ROUTING_FILE -- start the proxy with CONFIG_PATH=$ROUTING_FILE"
  echo "note: the fresh-bank rule and the hot-reload target are derived from this file at check time"
  exit 0
fi

UPSTREAM_TOKEN="${UPSTREAM_TOKEN:?set UPSTREAM_TOKEN to the Hindsight management token}"

[ -f "$ROUTING_FILE" ] || { echo "no $ROUTING_FILE; run with --write-config first" >&2; exit 2; }

# Everything the checks need is read back out of the routing table, keyed by
# agent name, so the script and the proxy can never disagree about which token
# means which bank. Positional indexing would break the moment a rule is added.
# Two columns per agent: token, then bank.
read -r TOKEN_A BANK_A TOKEN_B BANK_B TOKEN_FRESH BANK_FRESH TOKEN_OWN _ TOKEN_TRIM _ < <(
  python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
by_agent={e["agent"]:(t,e) for t,e in d["tokens"].items()}
cells=[]
for agent in ("E2eAlpha","E2eBeta","E2eFresh","E2eOwn","E2eTrim"):
    if agent not in by_agent:
        print("missing rule for agent %s; regenerate with --write-config" % agent, file=sys.stderr)
        sys.exit(2)
    t,e=by_agent[agent]
    cells += [t, e["bank"]]
print("\t".join(cells))
' "$ROUTING_FILE"
)
[ -n "${TOKEN_TRIM:-}" ] && [ -n "${BANK_FRESH:-}" ] || {
  echo "$ROUTING_FILE is not the table this script expects; regenerate it with --write-config" >&2; exit 2; }

pass=0; fail=0
ok()  { printf '  PASS  %s\n' "$1"; pass=$((pass+1)); }
bad() { printf '  FAIL  %s\n' "$1"; fail=$((fail+1)); }
check(){ if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (want $3, got $2)"; fi }
note(){ printf '  ..    %s\n' "$1"; }

# mcp <token> <method> [json-params]
mcp() {
  local tok="$1" method="$2" params="${3:-{\}}"
  curl -s -X POST "$PROXY/mcp" \
    -H "Authorization: Bearer $tok" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$method\",\"params\":$params}"
}

# tool <token> <tool> <json-args> -> prints the tool's text content
tool() {
  local tok="$1" name="$2" args="$3"
  mcp "$tok" tools/call "{\"name\":\"$name\",\"arguments\":$args}" \
    | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d["result"]["content"][0]["text"])'
}

# upstream <path> -> raw JSON from the Hindsight API using the management token
upstream() {
  curl -s -H "Authorization: Bearer $UPSTREAM_TOKEN" "$UPSTREAM$1"
}

# doc_tags <bank> <exact submitted text> -> the tags of the stored document
# whose content hash matches that text, or "" when no such document exists.
#
# Content hash is sha256 of the text exactly as submitted, and the documents
# endpoint echoes the tags verbatim. That makes this the storage-layer answer to
# "what did the proxy actually write" -- unlike memory text, which Hindsight's
# LLM fact extraction rewrites, so a literal search over memories would miss the
# probe entirely.
doc_tags() {
  local bank="$1" text="$2" want
  want=$(printf '%s' "$text" | sha256sum | awk '{print $1}')
  upstream "/v1/default/banks/$bank/documents" | python3 -c '
import json,sys
want = sys.argv[1]
d = json.load(sys.stdin)
for i in d.get("items", []):
    if i.get("content_hash") == want:
        print(json.dumps(i.get("tags") or []))
        break
' "$want"
}

http_code() {
  curl -s -o /dev/null -w '%{http_code}' -X POST "$PROXY/mcp" \
    -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" "$@"
}

# recall_count <token> <query> -> how many results came back, 0 on any failure
recall_count() {
  tool "$1" recall "{\"query\":\"$2\"}" 2>/dev/null \
    | python3 -c 'import sys,json; print(len(json.load(sys.stdin).get("results") or []))' 2>/dev/null \
    || echo 0
}

# wait_for_results <token> <query> <tries> -> the count once it is non-zero.
# Hindsight derives memories from a retained document asynchronously, so a read
# issued immediately after a write can legitimately come back empty.
wait_for_results() {
  local n=0
  for _ in $(seq 1 "$3"); do
    n=$(recall_count "$1" "$2")
    [ "${n:-0}" -gt 0 ] && break
    sleep 2
  done
  echo "${n:-0}"
}

echo "== AC1: unknown token is rejected, known token is accepted =="
code=$(http_code -H "Authorization: Bearer not-a-configured-token" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}')
check "unknown token -> 401" "$code" "401"
code=$(http_code -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}')
check "missing Authorization -> 401" "$code" "401"
code=$(http_code -H "Authorization: Bearer $TOKEN_A" -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}')
check "known token -> 200" "$code" "200"

# The proxy is only the single entry point if the caller's own token is useless
# against Hindsight directly.
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN_A" "$UPSTREAM/v1/default/banks")
check "caller token is rejected by Hindsight directly" "$code" "401"

echo "== AC2: two tokens, two banks, no cross-visibility =="
tool "$TOKEN_A" retain '{"items":[{"content":"E2E marker alpha: the ops deploy key rotates on Friday."}]}' >/dev/null
tool "$TOKEN_B" retain '{"items":[{"content":"E2E marker beta: the shared calendar is public."}]}' >/dev/null
a=$(tool "$TOKEN_A" recall '{"query":"shared calendar public"}' | tr 'A-Z' 'a-z')
b=$(tool "$TOKEN_B" recall '{"query":"ops deploy key rotate Friday"}' | tr 'A-Z' 'a-z')
case "$a" in *calendar*) bad "token A saw token B's memory" ;; *) ok "token A cannot see token B's bank" ;; esac
case "$b" in *deploy*key*) bad "token B saw token A's memory" ;; *) ok "token B cannot see token A's bank" ;; esac
# Each write must have landed in its own bank, checked at the storage layer
# rather than through the proxy that routed it. The documents endpoint keys on
# sha256 of the submitted text and echoes the tags verbatim, so it answers
# "what was actually written" without depending on LLM fact extraction, which
# rewrites the text of the memories it derives.
check "alpha's write is in $BANK_A upstream" \
  "$(doc_tags "$BANK_A" "$MARKER_ALPHA" | grep -c .)" "1"
check "beta's write is in $BANK_B upstream" \
  "$(doc_tags "$BANK_B" "$MARKER_BETA" | grep -c .)" "1"

echo "== AC3: a forged ownership tag is stripped before the write =="
tool "$TOKEN_B" retain "{\"items\":[{\"content\":\"$MARKER_FORGE\",\"tags\":[\"agent:E2eAlpha\",\"AGENT:E2eAlpha\",\" agent:E2eAlpha \",\"project:e2e\"]}]}" >/dev/null
# Read the stored document, not the proxy's response: the question is what is
# in the database, and a legitimately-tagged probe would be indistinguishable
# in the response alone.
stored=$(doc_tags "$BANK_B" "$MARKER_FORGE")
note "stored tags: $stored"
case "$stored" in
  "") bad "the probe never reached the store" ;;
  *agent:E2eAlpha*) bad "a forged agent tag reached the store: $stored" ;;
  *'"agent:E2eBeta"'*) ok "forgery stripped, real ownership tag injected" ;;
  *) bad "the caller's real ownership tag is missing: $stored" ;;
esac
case "$stored" in
  *project:e2e*) ok "legitimate non-agent tag preserved" ;;
  *) bad "a legitimate non-agent tag was dropped: $stored" ;;
esac

echo "== AC4: the tool surface is trimmed per token =="
full=$(mcp "$TOKEN_B" tools/list | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["result"]["tools"]))')
check "wildcard token sees all 6 tools" "$full" "6"
trimmed=$(mcp "$TOKEN_TRIM" tools/list | python3 -c 'import sys,json; print(",".join(sorted(t["name"] for t in json.load(sys.stdin)["result"]["tools"])))')
check "trimmed token sees only its list" "$trimmed" "list_tags,recall"
call=$(mcp "$TOKEN_TRIM" tools/call '{"name":"retain","arguments":{"items":[{"content":"must not be stored"}]}}')
case "$call" in *'"error"'*) ok "a trimmed-away tool is refused, not merely hidden" ;; *) bad "a trimmed-away tool was callable: $call" ;; esac

# Three readings of "the tool surface", because the issue's wording and what
# actually goes over the wire differ. See docs/verification.md.
mcp "$TOKEN_B" tools/list | python3 -c '
import json,sys
d=json.load(sys.stdin); tools=d["result"]["tools"]
c=lambda o: len(json.dumps(o,separators=(",",":")))
ds=sum(c(t["description"])+c(t["inputSchema"]) for t in tools)
nds=ds+sum(c(t["name"]) for t in tools)
wire=len(json.dumps(tools,separators=(",",":")))
print("  ..    description+schema: %d | +names: %d | tools/list wire bytes: %d" % (ds,nds,wire))
'

echo "== AC5: a routing-table change takes effect without a restart =="
before=$(mcp "$TOKEN_B" tools/list | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["result"]["tools"]))')
hot_token=$(openssl rand -hex 24)
# Two live changes at once: a brand-new token (proves rules are added) and an edit
# of an existing rule (proves a changed rule replaces the old one). The edit flips
# E2eTrim to the wildcard, which is observable in its tool list; E2eOwn is left
# alone on purpose, because its read scope is exercised further down.
python3 - "$ROUTING_FILE" "$hot_token" "$BANK_HOT" <<'EOF'
import json,sys
path,tok,bank = sys.argv[1],sys.argv[2],sys.argv[3]
d=json.load(open(path))
d["tokens"][tok]={"agent":"E2eHot","bank":bank,"tools":["recall"]}
for e in d["tokens"].values():
    if e.get("agent") == "E2eTrim":
        e["tools"] = "*"
json.dump(d,open(path,"w"),indent=2)
EOF
note "added a token and flipped E2eTrim to tools:\"*\" in $ROUTING_FILE"
for _ in $(seq 1 "$RELOAD_WAIT"); do
  hot_code=$(http_code -H "Authorization: Bearer $hot_token" \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}')
  [ "$hot_code" = "200" ] && break
  sleep 1
done
check "the new token is live with no restart" "$hot_code" "200"
hot_tools=$(mcp "$hot_token" tools/list | python3 -c 'import sys,json; print(",".join(t["name"] for t in json.load(sys.stdin)["result"]["tools"]))' 2>/dev/null)
check "the new token's tool list is trimmed as configured" "$hot_tools" "recall"
# The edited rule must be live too: a check that only adds a token would not
# notice an edit that never replaced the previous rule.
flipped=$(mcp "$TOKEN_TRIM" tools/list | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["result"]["tools"]))' 2>/dev/null)
check "the edited rule took effect without a restart" "$flipped" "6"
after=$(mcp "$TOKEN_B" tools/list | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["result"]["tools"]))')
check "the pre-existing token is unaffected by the reload" "$after" "$before"
# The reload added a token; it must not have dropped the old ones.
code=$(http_code -H "Authorization: Bearer $TOKEN_A" -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}')
check "the reload kept the existing rules" "$code" "200"

echo "== AC6: a bank that does not exist is created on demand =="
# GET on a single bank is 405 upstream (it only accepts PUT/PATCH/DELETE), so
# existence is read from the bank list instead.
bank_exists() {
  upstream "/v1/default/banks" | python3 -c '
import json,sys
print("yes" if any(b["bank_id"] == sys.argv[1] for b in json.load(sys.stdin).get("banks", [])) else "no")
' "$1"
}
check "$BANK_FRESH does not exist beforehand" "$(bank_exists "$BANK_FRESH")" "no"
out=$(tool "$TOKEN_FRESH" retain '{"items":[{"content":"E2E auto-create probe."}]}' 2>&1)
case "$out" in *'"success": true'*) ok "write into a fresh bank succeeded" ;; *) bad "fresh-bank write failed: $out" ;; esac
check "the bank now exists upstream" "$(bank_exists "$BANK_FRESH")" "yes"

echo "== read_scope: own hides other writers in the same bank =="
# Both probes are written to the same bank. The own-scoped caller must see only
# its own. The assertions go through the proxy's recall, which is the behaviour
# under test; the storage layer would show both regardless.
#
# Hindsight derives memories from a retained document with an LLM, asynchronously
# and with a latency of ten-odd seconds, and it rewrites the text while doing so.
# So the checks wait for extraction rather than racing it, and they assert on the
# tags that come back instead of on the rewritten text.
tool "$TOKEN_B" retain "{\"items\":[{\"content\":\"$MARKER_SCOPE_B\"}]}" >/dev/null
tool "$TOKEN_OWN" retain "{\"items\":[{\"content\":\"$MARKER_OWN\"}]}" >/dev/null

own_n=$(wait_for_results "$TOKEN_OWN" "quarterly review" 20)
shared_n=$(wait_for_results "$TOKEN_B" "quarterly review" 20)
note "after extraction: own-scoped caller saw $own_n, shared-scoped control saw $shared_n"

check "an own-scoped caller sees its own memory" \
  "$([ "${own_n:-0}" -gt 0 ] && echo yes || echo no)" "yes"
# The control matters: without it, a pass above could just mean nothing was
# extracted yet.
check "a shared-scoped caller on that bank still sees everything" \
  "$([ "${shared_n:-0}" -gt 0 ] && echo yes || echo no)" "yes"

# Nothing outside the caller's own ownership tag may come back, whatever the text
# was rewritten to.
leaked=$(tool "$TOKEN_OWN" recall '{"query":"quarterly review"}' | python3 -c '
import json,sys
d=json.load(sys.stdin)
print(len([r for r in (d.get("results") or []) if "agent:E2eOwn" not in (r.get("tags") or [])]))
')
check "an own-scoped caller sees only its own tag" "$leaked" "0"
# Bank path handling: a caller must not be able to name a bank itself.
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$PROXY/mcp/$BANK_A" \
  -H "Authorization: Bearer $TOKEN_A" -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}')
check "a bank named in the URL path is not routable" "$code" "404"

echo
printf 'passed %d, failed %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
