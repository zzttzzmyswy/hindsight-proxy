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
# Bank for the AC5 rule that gets edited in place. It belongs to E2eSpare, a
# token no other check uses, so editing it cannot disturb a later section.
BANK_SPARE="spare-e2e"
# Bank for the auto-create check. The id is generated when the table is written,
# so it is absent at the start of a fresh run.
BANK_FRESH="fresh-e2e-$RANDOM$RANDOM"

# Probe texts are written out in full and hashed as-is, so the storage-layer
# check can find the document by content hash.
MARKER_ALPHA="E2E marker alpha: the ops deploy key rotates on Friday."
MARKER_BETA="E2E marker beta: the shared calendar is public."
MARKER_FORGE="E2E forged-tag probe: the staging password is rotated monthly."
MARKER_OWN="E2E own-scope probe: a private note about the quarterly review."
MARKER_SCOPE_B="E2E shared-scope probe: a public note about the quarterly review."

# --write-config runs before the upstream token is needed, so the table can be
# generated (and the proxy started) without one.
if [ "${1:-}" = "--write-config" ]; then
  cat >"$ROUTING_FILE" <<EOF
{
  "default_bank": "$BANK_B",
  "tokens": {
    "$(openssl rand -hex 24)": { "agent": "E2eAlpha", "bank": "$BANK_A",     "tools": "*" },
    "$(openssl rand -hex 24)": { "agent": "E2eBeta",  "bank": "$BANK_B",     "tools": "*" },
    "$(openssl rand -hex 24)": { "agent": "E2eFresh", "bank": "$BANK_FRESH", "tools": "*" },
    "$(openssl rand -hex 24)": { "agent": "E2eOwn",   "bank": "$BANK_B",     "tools": "*", "read_scope": "own" },
    "$(openssl rand -hex 24)": { "agent": "E2eTrim",  "bank": "$BANK_B",     "tools": ["recall", "list_tags"] },
    "$(openssl rand -hex 24)": { "agent": "E2eSpare", "bank": "$BANK_SPARE", "tools": ["recall"] }
  }
}
EOF
  echo "wrote $ROUTING_FILE -- start the proxy with CONFIG_PATH=$ROUTING_FILE"
  exit 0
fi

UPSTREAM_TOKEN="${UPSTREAM_TOKEN:?set UPSTREAM_TOKEN to the Hindsight management token}"

[ -f "$ROUTING_FILE" ] || { echo "no $ROUTING_FILE; run with --write-config first" >&2; exit 2; }

# Everything the checks need is read back out of the routing table, keyed by
# agent name, so the script and the proxy can never disagree about which token
# means which bank. Positional indexing would break the moment a rule is added.
# Two columns per agent: token, then bank.
read -r TOKEN_A BANK_A TOKEN_B BANK_B TOKEN_FRESH BANK_FRESH TOKEN_OWN _ TOKEN_TRIM _ TOKEN_SPARE BANK_SPARE < <(
  python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
by_agent={e["agent"]:(t,e) for t,e in d["tokens"].items()}
cells=[]
for agent in ("E2eAlpha","E2eBeta","E2eFresh","E2eOwn","E2eTrim","E2eSpare"):
    if agent not in by_agent:
        print("missing rule for agent %s; regenerate with --write-config" % agent, file=sys.stderr)
        sys.exit(2)
    t,e=by_agent[agent]
    cells += [t, e["bank"]]
print("\t".join(cells))
' "$ROUTING_FILE"
)
if [ -z "${TOKEN_SPARE:-}" ] || [ -z "${BANK_FRESH:-}" ]; then
  echo "$ROUTING_FILE is not the table this script expects; regenerate it with --write-config" >&2
  exit 2
fi

pass=0; fail=0; skip=0
ok()  { printf '  PASS  %s\n' "$1"; pass=$((pass+1)); }
bad() { printf '  FAIL  %s\n' "$1"; fail=$((fail+1)); }
check(){ if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (want $3, got $2)"; fi }
note(){ printf '  ..    %s\n' "$1"; }
skipped(){ printf '  SKIP  %s\n' "$1"; skip=$((skip+1)); }

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

# tagged_count <bank> <tag> -> how many stored memories carry that tag
tagged_count() {
  upstream "/v1/default/banks/$1/memories/list?limit=200" | python3 -c '
import json,sys
tag = sys.argv[1]
d = json.load(sys.stdin)
print(len([i for i in (d.get("items") or []) if tag in (i.get("tags") or [])]))
' "$2"
}

# wait_for_tagged <bank> <tag> <tries> -> the count once it is non-zero.
#
# Hindsight derives memories from a retained document asynchronously (ten-odd
# seconds against a real LLM), so a read issued right after a write can
# legitimately come back empty. The wait reads the STORAGE layer with the
# management token rather than polling through the proxy: this helper is used
# before the own-scope assertions, and a burst of reads through a caller token
# while extraction is still in flight makes the outcome depend on read timing.
wait_for_tagged() {
  local n=0
  for _ in $(seq 1 "$3"); do
    n=$(tagged_count "$1" "$2")
    [ "${n:-0}" -gt 0 ] && break
    sleep 2
  done
  echo "${n:-0}"
}

# recall_count <token> <query> -> how many results came back, 0 on any failure
recall_count() {
  tool "$1" recall "{\"query\":\"$2\"}" 2>/dev/null \
    | python3 -c 'import sys,json; print(len(json.load(sys.stdin).get("results") or []))' 2>/dev/null \
    || echo 0
}

http_code() {
  curl -s -o /dev/null -w '%{http_code}' -X POST "$PROXY/mcp" \
    -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" "$@"
}

bank_exists() {
  upstream "/v1/default/banks" | python3 -c '
import json,sys
print("yes" if any(b["bank_id"] == sys.argv[1] for b in json.load(sys.stdin).get("banks", [])) else "no")
' "$1"
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
tool "$TOKEN_A" retain "{\"items\":[{\"content\":\"$MARKER_ALPHA\"}]}" >/dev/null
tool "$TOKEN_B" retain "{\"items\":[{\"content\":\"$MARKER_BETA\"}]}" >/dev/null
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
  "$([ -n "$(doc_tags "$BANK_A" "$MARKER_ALPHA")" ] && echo yes || echo no)" "yes"
check "beta's write is in $BANK_B upstream" \
  "$([ -n "$(doc_tags "$BANK_B" "$MARKER_BETA")" ] && echo yes || echo no)" "yes"

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
# actually goes over the wire differ. Compact separators match Go's json.Marshal
# -- what the proxy emits and what ToolSurfaceSize measures. A pretty-printer
# reports the same payload larger (2801 / 2864), which is a formatting
# difference, not a different surface. See docs/verification.md.
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
# Two live changes at once: a brand-new token (proves rules are added) and an
# edit of an existing rule (proves a changed rule replaces the old one). The edit
# flips E2eSpare to the wildcard, which is observable in its tool list. It
# targets E2eSpare -- a token no other check reads -- so that running this script
# twice against the same deployment still starts from the state AC4 expects.
python3 - "$ROUTING_FILE" "$hot_token" "$BANK_HOT" <<'EOF'
import json,sys
path,tok,bank = sys.argv[1],sys.argv[2],sys.argv[3]
d=json.load(open(path))
d["tokens"][tok]={"agent":"E2eHot","bank":bank,"tools":["recall"]}
for e in d["tokens"].values():
    if e.get("agent") == "E2eSpare":
        e["tools"] = "*"
json.dump(d,open(path,"w"),indent=2)
EOF
note "added a token and flipped E2eSpare to tools:\"*\" in $ROUTING_FILE"
hot_code=""
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
flipped=$(mcp "$TOKEN_SPARE" tools/list | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["result"]["tools"]))' 2>/dev/null)
check "the edited rule took effect without a restart" "$flipped" "6"
after=$(mcp "$TOKEN_B" tools/list | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["result"]["tools"]))')
check "the pre-existing token is unaffected by the reload" "$after" "$before"
# The reload added a token; it must not have dropped the old ones.
code=$(http_code -H "Authorization: Bearer $TOKEN_A" -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}')
check "the reload kept the existing rules" "$code" "200"

echo "== AC6: a bank that does not exist is created on demand =="
# GET on a single bank is 405 upstream (it only accepts PUT/PATCH/DELETE), so
# existence is read from the bank list instead.
if [ "$(bank_exists "$BANK_FRESH")" = "no" ]; then
  ok "$BANK_FRESH does not exist beforehand"
else
  # The id is minted with the table, so this only happens when the script is
  # re-run against a deployment that already ran once. The property under test
  # is still checked below.
  skipped "$BANK_FRESH does not exist beforehand (bank left by an earlier run)"
fi
out=$(tool "$TOKEN_FRESH" retain '{"items":[{"content":"E2E auto-create probe."}]}' 2>&1)
case "$out" in *'"success": true'*) ok "write into a fresh bank succeeded" ;; *) bad "fresh-bank write failed: $out" ;; esac
check "the bank now exists upstream" "$(bank_exists "$BANK_FRESH")" "yes"

echo "== read_scope: own hides other writers in the same bank =="
# Both probes go to the same bank. The own-scoped caller must see only its own.
#
# The wait is on the STORAGE layer, with the management token, and that is
# deliberate. Extraction is asynchronous, and polling through the own-scoped
# token while it is still in flight makes the result depend on read timing, so
# the read path is exercised once, after the data has settled.
tool "$TOKEN_B" retain "{\"items\":[{\"content\":\"$MARKER_SCOPE_B\"}]}" >/dev/null
tool "$TOKEN_OWN" retain "{\"items\":[{\"content\":\"$MARKER_OWN\"}]}" >/dev/null

own_settled=$(wait_for_tagged "$BANK_B" "agent:E2eOwn" 30)
note "waited upstream for extraction: $own_settled memories carry agent:E2eOwn"
if [ "${own_settled:-0}" -eq 0 ]; then
  bad "no memory upstream carries agent:E2eOwn after 60s -- the own-scoped write produced no own-tagged memory"
else
  ok "the own-scoped write produced an own-tagged memory upstream"
fi

# The own-scoped caller's own memory must come back.
own_n=$(recall_count "$TOKEN_OWN" "quarterly review")
check "an own-scoped caller sees its own memory" \
  "$([ "${own_n:-0}" -gt 0 ] && echo yes || echo no)" "yes"

# Nothing outside the caller's own ownership tag may come back, whatever the
# text was rewritten to.
leaked=$(tool "$TOKEN_OWN" recall '{"query":"quarterly review"}' | python3 -c '
import json,sys
d=json.load(sys.stdin)
print(len([r for r in (d.get("results") or []) if "agent:E2eOwn" not in (r.get("tags") or [])]))
')
check "an own-scoped caller sees only its own tag" "$leaked" "0"

# Report any ownership tag the proxy did not inject. Hindsight's legacy
# agent_tool_policy extension stamps each write with the identity of the token it
# authenticated as, and the proxy always uses the admin token -- so while that
# extension is enabled every memory also picks up `agent:<admin>`. That is
# upstream policy rather than a proxy failure, but it pollutes ownership auditing
# and read isolation, so surface it instead of leaving it to be found by hand.
extra_owners=$(upstream "/v1/default/banks/$BANK_B/tags" | python3 -c '
import json,sys
known = {"agent:E2eOwn", "agent:E2eBeta", "project:e2e"}
extra = [t["tag"] for t in json.load(sys.stdin).get("items", [])
         if t["tag"].startswith("agent:") and t["tag"] not in known]
print(", ".join(extra))
')
if [ -n "$extra_owners" ]; then
  note "WARNING: ownership tags not injected by this proxy are present: $extra_owners"
  note "  source is a Hindsight extension (legacy agent_tool_policy), not the proxy"
fi

# The control has to prove the bank really does hold another writer's memories;
# otherwise the checks above could pass simply because nothing is there. A
# shared-scoped caller on the same bank must see entries that are NOT the own
# caller's, which is exactly what the own caller is being denied.
shared_foreign=$(tool "$TOKEN_B" recall '{"query":"quarterly review"}' | python3 -c '
import json,sys
d=json.load(sys.stdin)
print(len([r for r in (d.get("results") or []) if "agent:E2eOwn" not in (r.get("tags") or [])]))
')
check "a shared-scoped caller on that bank still sees other writers" \
  "$([ "${shared_foreign:-0}" -gt 0 ] && echo yes || echo no)" "yes"
# Bank path handling: a caller must not be able to name a bank itself.
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$PROXY/mcp/$BANK_A" \
  -H "Authorization: Bearer $TOKEN_A" -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}')
check "a bank named in the URL path is not routable" "$code" "404"

echo
printf 'passed %d, failed %d, skipped %d\n' "$pass" "$fail" "$skip"
[ "$fail" -eq 0 ]
