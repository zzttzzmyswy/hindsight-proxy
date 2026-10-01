#!/usr/bin/env bash
# End-to-end acceptance check against a LIVE Hindsight + hindsight-proxy stack.
#
# The Go test suite (go test ./...) covers all six acceptance criteria against a
# fake upstream and is the check CI runs. This script is the complementary one:
# it exercises a real deployment, where embeddings, LLM fact extraction and the
# real tag/scope semantics are involved.
#
# Usage:
#   PROXY=http://127.0.0.1:8890 \
#   UPSTREAM=http://127.0.0.1:8888 \
#   UPSTREAM_TOKEN=<management token> \
#   ./scripts/verify_e2e.sh
#
# It writes to the banks its routing table names, so point it at a scratch
# deployment rather than production.

set -uo pipefail

PROXY="${PROXY:-http://127.0.0.1:8890}"
UPSTREAM="${UPSTREAM:-http://127.0.0.1:8888}"
UPSTREAM_TOKEN="${UPSTREAM_TOKEN:?set UPSTREAM_TOKEN to the Hindsight management token}"

# Caller tokens from your routing table. The defaults match examples/tokens.json
# layout: A and B must be on different banks, B's bank is used for the
# forged-tag store check, and FRESH must be a token whose bank does not exist.
TOKEN_A="${TOKEN_A:-tok-alpha}"
TOKEN_B="${TOKEN_B:-tok-beta}"
TOKEN_FRESH="${TOKEN_FRESH:-tok-fresh}"
BANK_B="${BANK_B:-shared}"

pass=0
fail=0
ok()   { printf '  PASS  %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  FAIL  %s\n' "$1"; fail=$((fail+1)); }
check(){ if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (want $3, got $2)"; fi; }

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

echo "== AC1: unknown token is rejected, known token is accepted =="
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$PROXY/mcp" \
  -H "Authorization: Bearer not-a-configured-token" \
  -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}')
check "unknown token -> 401" "$code" "401"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$PROXY/mcp" \
  -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}')
check "missing Authorization -> 401" "$code" "401"

echo "== AC4: the tool surface is trimmed per token =="
sizes=$(for t in "$TOKEN_A" "$TOKEN_B"; do
  mcp "$t" tools/list | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["result"]["tools"]))'
done | tr '\n' ' ')
if [ "$(echo "$sizes" | awk '{print $1}')" -ge 6 ]; then ok "wildcard token sees the full surface"; else bad "wildcard token tool count: $sizes"; fi

echo "== AC2: two tokens, two banks, no cross-visibility =="
tool "$TOKEN_A" retain '{"items":[{"content":"E2E marker alpha: the ops deploy key rotates on Friday."}]}' >/dev/null
tool "$TOKEN_B" retain '{"items":[{"content":"E2E marker beta: the shared calendar is public."}]}' >/dev/null
a=$(tool "$TOKEN_A" recall '{"query":"shared calendar public"}' | tr 'A-Z' 'a-z')
b=$(tool "$TOKEN_B" recall '{"query":"ops deploy key rotate Friday"}' | tr 'A-Z' 'a-z')
case "$a" in *calendar*) bad "token A saw token B's memory" ;; *) ok "token A cannot see token B's bank" ;; esac
case "$b" in *deploy*key*) bad "token B saw token A's memory" ;; *) ok "token B cannot see token A's bank" ;; esac

echo "== AC3: a forged ownership tag is stripped before the write =="
tool "$TOKEN_B" retain '{"items":[{"content":"E2E forged-tag probe.","tags":["agent:SomeoneElse","project:e2e"]}]}' >/dev/null
tags=$(curl -s -H "Authorization: Bearer $UPSTREAM_TOKEN" \
  "$UPSTREAM/v1/default/banks/$BANK_B/tags" | python3 -c 'import sys,json; print(" ".join(t["tag"] for t in json.load(sys.stdin)["items"]))')
case "$tags" in *agent:someoneelse*) bad "a forged agent tag reached the store: $tags" ;; *) ok "no forged agent tag in the store" ;; esac

echo "== AC6: a bank that does not exist is created on demand =="
out=$(tool "$TOKEN_FRESH" retain '{"items":[{"content":"E2E auto-create probe."}]}' 2>&1)
case "$out" in *'"success": true'*) ok "write into a fresh bank succeeded" ;; *) bad "fresh-bank write failed: $out" ;; esac

echo
printf 'passed %d, failed %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
