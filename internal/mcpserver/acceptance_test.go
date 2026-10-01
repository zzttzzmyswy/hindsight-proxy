package mcpserver_test

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/zzttzzmyswy/hindsight-proxy/internal/config"
	"github.com/zzttzzmyswy/hindsight-proxy/internal/mcpserver"
)

// routingTable is the shared fixture: two callers on two banks, a caller scoped
// to its own writes, a caller with a trimmed tool list, and a caller with no
// agent name whose writes therefore carry no ownership tag.
const routingTable = `{
  "default_bank": "shared",
  "tokens": {
    "token-alpha":  {"agent": "Mika",   "bank": "ops",    "tools": "*"},
    "token-beta":   {"agent": "Riven",  "bank": "shared", "tools": "*"},
    "token-own":    {"agent": "Solo",   "bank": "shared", "read_scope": "own", "tools": "*"},
    "token-legacy": {"bank": "shared", "tools": "*"},
    "token-trim":   {"agent": "Narrow", "bank": "shared", "tools": ["recall", "list_tags"]}
  }
}`

// routingTableUpdated is the same table after an operator edits it in place: a
// brand-new caller is added and an existing one is re-pointed to another bank.
// It is written as a whole document rather than patched, so the test exercises a
// realistic edit instead of a substring hazard.
const routingTableUpdated = `{
  "default_bank": "shared",
  "tokens": {
    "token-alpha": {"agent": "Mika",  "bank": "moved",  "tools": "*", "description": "ops agent"},
    "token-beta":  {"agent": "Riven", "bank": "shared", "tools": "*", "description": "general agent"},
    "token-own":   {"agent": "Solo",  "bank": "shared", "read_scope": "own", "tools": "*"},
    "token-trim":  {"agent": "Narrow", "bank": "shared", "tools": ["recall", "list_tags"]},
    "token-new":   {"agent": "Newcomer", "bank": "later", "tools": ["recall"]}
  }
}`

// --- Acceptance 1: unknown token is rejected; known token routes to its bank ----

func TestUnknownTokenIsRejectedWithoutFallingBack(t *testing.T) {
	s := newTestServer(t, routingTable)

	resp := s.call("not-a-configured-token", "tools/list", map[string]any{})
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.status)
	}
	if !strings.Contains(resp.raw, "unknown token") {
		t.Fatalf("body = %q, want it to name the reason", resp.raw)
	}

	// A missing header is the same rejection, not an anonymous default.
	resp = s.call("", "tools/list", map[string]any{})
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d, want 401", resp.status)
	}
}

func TestBareTokenHeaderIsRejected(t *testing.T) {
	s := newTestServer(t, routingTable)

	// The scheme is required; a raw token in Authorization is not a bearer token.
	resp := s.callWithAuthHeader(alphaToken, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.status)
	}
}

func TestKnownTokenRoutesToItsBank(t *testing.T) {
	s := newTestServer(t, routingTable)

	if text, isErr := s.callTool(alphaToken, "retain", map[string]any{
		"items": []any{map[string]any{"content": "alpha wrote this"}},
	}); isErr {
		t.Fatalf("retain failed: %s", text)
	}

	// The write must have landed on the bank the routing table names, not on
	// the default bank and not on a bank the caller could have chosen.
	writes := s.fake.writesTo("ops")
	if len(writes) != 1 {
		t.Fatalf("ops bank saw %d writes, want 1; all paths: %v", len(writes), s.fake.requestPaths())
	}
	if got := s.fake.writesTo("shared"); len(got) != 0 {
		t.Fatalf("shared bank saw %d writes, want 0", len(got))
	}
}

// --- Acceptance 2: two tokens, two banks, no cross-visibility ------------------

func TestTwoTokensOnDifferentBanksCannotSeeEachOther(t *testing.T) {
	s := newTestServer(t, routingTable)

	if text, isErr := s.callTool(alphaToken, "retain", map[string]any{
		"items": []any{map[string]any{"content": "alpha secret: the ops deploy key rotates on Friday"}},
	}); isErr {
		t.Fatalf("alpha retain failed: %s", text)
	}
	if text, isErr := s.callTool(betaToken, "retain", map[string]any{
		"items": []any{map[string]any{"content": "beta secret: the shared calendar is public"}},
	}); isErr {
		t.Fatalf("beta retain failed: %s", text)
	}

	// Each caller recalls something only the other could have written.
	alphaSees, _ := s.callTool(alphaToken, "recall", map[string]any{"query": "deploy key rotation"})
	if !strings.Contains(alphaSees, "ops deploy key") {
		t.Fatalf("alpha cannot see its own memory: %s", alphaSees)
	}
	if strings.Contains(alphaSees, "shared calendar") {
		t.Fatalf("alpha can see beta's memory across banks: %s", alphaSees)
	}

	betaSees, _ := s.callTool(betaToken, "recall", map[string]any{"query": "calendar visibility"})
	if !strings.Contains(betaSees, "shared calendar") {
		t.Fatalf("beta cannot see its own memory: %s", betaSees)
	}
	if strings.Contains(betaSees, "ops deploy key") {
		t.Fatalf("beta can see alpha's memory across banks: %s", betaSees)
	}
}

// --- Acceptance 3: ownership tag cannot be forged -----------------------------

func TestCallerSuppliedAgentTagsAreStrippedAndReplaced(t *testing.T) {
	s := newTestServer(t, routingTable)

	// The caller tries to write as someone else, in several case variants, while
	// also sending a tag it is entitled to keep.
	if text, isErr := s.callTool(betaToken, "retain", map[string]any{
		"items": []any{map[string]any{
			"content": "forged ownership attempt",
			"tags":    []any{"agent:Mika", "AGENT:Mika", " agent:Mika", "project:apollo"},
		}},
	}); isErr {
		t.Fatalf("retain failed: %s", text)
	}

	written := s.fake.tokensWrittenTo("shared")
	if len(written) != 1 {
		t.Fatalf("wrote %d items, want 1", len(written))
	}
	tags := written[0]

	for _, forged := range []string{"agent:Mika", "AGENT:Mika", " agent:Mika"} {
		if contains(tags, forged) {
			t.Fatalf("forged tag %q survived to the upstream request: %v", forged, tags)
		}
	}
	if !contains(tags, "agent:Riven") {
		t.Fatalf("the caller's real ownership tag is missing: %v", tags)
	}
	if !contains(tags, "project:apollo") {
		t.Fatalf("a legitimate non-agent tag was dropped: %v", tags)
	}
}

// An own-scoped caller must not see memories written by others in the same bank,
// even when those memories carry no ownership tag at all.
//
// This is the case a request-side tag filter alone cannot cover: upstream's
// "any" matching also returns untagged memories, so a filter of [agent:Solo]
// still matches anything the untagged writer stored. Only filtering the response
// removes it.
func TestReadScopeOwnHidesOtherWritersInTheSameBank(t *testing.T) {
	s := newTestServer(t, routingTable)

	// Riven writes a tagged memory into the shared bank.
	if _, isErr := s.callTool(betaToken, "retain", map[string]any{
		"items": []any{map[string]any{"content": "riven handover note"}},
	}); isErr {
		t.Fatal("seed from the tagged caller failed")
	}
	// The legacy caller has no agent, so its writes carry no ownership tag.
	if _, isErr := s.callTool("token-legacy", "retain", map[string]any{
		"items": []any{map[string]any{"content": "untagged legacy note"}},
	}); isErr {
		t.Fatal("seed from the untagged caller failed")
	}

	// Solo is on the same bank but scoped to its own writes.
	text, isErr := s.callTool("token-own", "recall", map[string]any{"query": "note"})
	if isErr {
		t.Fatalf("recall failed: %s", text)
	}
	if strings.Contains(text, "riven handover note") {
		t.Fatalf("an own-scoped caller read another agent's memory: %s", text)
	}
	if strings.Contains(text, "untagged legacy note") {
		t.Fatalf("an own-scoped caller read an untagged memory: %s", text)
	}

	// A shared-scoped caller on the same bank still sees both, so the test is
	// not passing because retrieval broke.
	shared, _ := s.callTool(betaToken, "recall", map[string]any{"query": "note"})
	if !strings.Contains(shared, "riven handover note") || !strings.Contains(shared, "untagged legacy note") {
		t.Fatalf("a shared-scoped caller lost visibility: %s", shared)
	}

	// Solo's own write is still visible to Solo.
	if _, isErr := s.callTool("token-own", "retain", map[string]any{
		"items": []any{map[string]any{"content": "solo own note"}},
	}); isErr {
		t.Fatal("own-scoped retain failed")
	}
	own, _ := s.callTool("token-own", "recall", map[string]any{"query": "note"})
	if !strings.Contains(own, "solo own note") {
		t.Fatalf("own-scoped caller cannot see its own memory: %s", own)
	}
	if strings.Contains(own, "legacy note") {
		t.Fatalf("own-scoped caller still sees the untagged memory: %s", own)
	}
}

// A caller must not be able to widen its own read scope by naming another
// agent's tag in the request.
func TestReadScopeTagFilterCannotBeWidenedByNamingAnotherAgent(t *testing.T) {
	s := newTestServer(t, routingTable)

	if _, isErr := s.callTool(betaToken, "retain", map[string]any{
		"items": []any{map[string]any{"content": "mika private note about the rollout"}},
	}); isErr {
		t.Fatal("seed failed")
	}

	// The "own"-scoped caller asks explicitly for another agent's tag.
	text, isErr := s.callTool("token-own", "recall", map[string]any{
		"query": "rollout note",
		"tags":  []any{"agent:Riven"},
	})
	if isErr {
		t.Fatalf("recall failed: %s", text)
	}
	if strings.Contains(text, "mika private note") {
		t.Fatalf("an own-scoped caller read another agent's memory: %s", text)
	}
}

// Browsing and single-record reads must respect the same scope as recall, or an
// own-scoped caller could read around its scope one id at a time.
func TestReadScopeOwnAppliesToListingAndGet(t *testing.T) {
	s := newTestServer(t, routingTable)

	if _, isErr := s.callTool(betaToken, "retain", map[string]any{
		"items": []any{map[string]any{"content": "riven browseable note"}},
	}); isErr {
		t.Fatal("seed failed")
	}
	if _, isErr := s.callTool("token-legacy", "retain", map[string]any{
		"items": []any{map[string]any{"content": "untagged browseable note"}},
	}); isErr {
		t.Fatal("seed failed")
	}

	listing, isErr := s.callTool("token-own", "list_memories", map[string]any{})
	if isErr {
		t.Fatalf("list_memories failed: %s", listing)
	}
	if strings.Contains(listing, "riven browseable note") || strings.Contains(listing, "untagged browseable note") {
		t.Fatalf("own-scoped listing leaked another writer's memory: %s", listing)
	}

	// A shared-scoped caller on the same bank still sees both, so the assertion
	// above is not passing because the listing is simply empty.
	shared, _ := s.callTool(betaToken, "list_memories", map[string]any{})
	if !strings.Contains(shared, "riven browseable note") || !strings.Contains(shared, "untagged browseable note") {
		t.Fatalf("shared-scoped listing lost visibility: %s", shared)
	}

	// Reading another writer's record by id must look like a miss, not a denial.
	id := s.fake.memoryID("shared", "riven browseable note")
	got, isErr := s.callTool("token-own", "get_memory", map[string]any{"memory_id": id})
	if !isErr {
		t.Fatalf("own-scoped get_memory returned another writer's record: %s", got)
	}
	if strings.Contains(got, "riven browseable") {
		t.Fatalf("the error message leaked the record: %s", got)
	}
}

// --- Acceptance 4: per-token tool trimming and the size budget -----------------

func TestToolListIsTrimmedPerToken(t *testing.T) {
	s := newTestServer(t, routingTable)

	all := s.toolNames(alphaToken)
	if !sortedEqual(all, []string{"get_memory", "list_memories", "list_tags", "recall", "reflect", "retain"}) {
		t.Fatalf("wildcard token sees %v, want all six tools", all)
	}

	trimmed := s.toolNames("token-trim")
	if !sortedEqual(trimmed, []string{"recall", "list_tags"}) {
		t.Fatalf("trimmed token sees %v, want [recall list_tags]", trimmed)
	}
}

// A tool a token may not use must be refused, not merely hidden from the list.
func TestTrimmedTokenCannotCallAnUnexposedTool(t *testing.T) {
	s := newTestServer(t, routingTable)

	// The call is rejected at the protocol level, which is what an unregistered
	// tool produces; either way it must never reach upstream.
	resp := s.call("token-trim", "tools/call", map[string]any{
		"name": "retain", "arguments": map[string]any{
			"items": []any{map[string]any{"content": "should not be stored"}},
		},
	})
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a JSON-RPC error (%s)", resp.status, resp.raw)
	}
	if resp.body.Error == nil {
		t.Fatal("a tool outside the token's list was callable")
	}
	if got := s.fake.writesTo("shared"); len(got) != 0 {
		t.Fatalf("a tool outside the token's list reached upstream: %d writes", len(got))
	}
}

// Destructive Hindsight tools must not exist on this surface at all.
func TestDestructiveToolsAreNeverExposed(t *testing.T) {
	s := newTestServer(t, routingTable)

	all := s.toolNames(alphaToken)
	for _, banned := range []string{"delete_bank", "clear_memories", "delete_document"} {
		if contains(all, banned) {
			t.Fatalf("destructive tool %q is exposed: %v", banned, all)
		}
	}
}

func TestToolSurfaceStaysWithinItsBudget(t *testing.T) {
	const budget = 3000
	if got := mcpserver.ToolSurfaceSize(); got >= budget {
		t.Fatalf("tool surface is %d characters, want < %d", got, budget)
	}
}

// --- Acceptance 5: the routing table hot-reloads -------------------------------

func TestRoutingTableChangeTakesEffectWithoutRestart(t *testing.T) {
	s := newTestServer(t, routingTable)

	if resp := s.call("token-new", "tools/list", map[string]any{}); resp.status != http.StatusUnauthorized {
		t.Fatalf("token-new worked before it was configured: %d", resp.status)
	}

	// Add a caller and re-point an existing one, without touching the process.
	if err := writeFile(s.cfgPath, routingTableUpdated); err != nil {
		t.Fatalf("rewrite routing table: %v", err)
	}

	waitFor(t, "the new token to be accepted", func() bool {
		return s.call("token-new", "tools/list", map[string]any{}).status == http.StatusOK
	})
	if names := s.toolNames("token-new"); !sortedEqual(names, []string{"recall"}) {
		t.Fatalf("new caller sees %v, want [recall]", names)
	}

	// The re-pointed caller must now write to its new bank.
	if _, isErr := s.callTool(alphaToken, "retain", map[string]any{
		"items": []any{map[string]any{"content": "written after the move"}},
	}); isErr {
		t.Fatal("retain after reload failed")
	}
	waitFor(t, "alpha's writes to land on the new bank", func() bool {
		return len(s.fake.writesTo("moved")) == 1
	})
	if got := s.fake.writesTo("ops"); len(got) != 0 {
		t.Fatalf("the old bank still received %d writes after the move", len(got))
	}
}

// --- Acceptance 6: the target bank is created on demand ------------------------

func TestBankIsCreatedOnDemand(t *testing.T) {
	s := newTestServer(t, routingTable)

	// Nothing pre-created "ops"; the first request must create it and succeed.
	if _, isErr := s.callTool(alphaToken, "retain", map[string]any{
		"items": []any{map[string]any{"content": "first write into a fresh bank"}},
	}); isErr {
		t.Fatal("retain into a non-existent bank failed")
	}

	paths := s.fake.requestPaths()
	if !contains(paths, "PUT /v1/default/banks/ops") {
		t.Fatalf("bank was never created; calls were: %v", paths)
	}

	// The bank is confirmed once, not probed on every request.
	before := countPrefix(paths, "PUT /v1/default/banks/ops")
	if _, isErr := s.callTool(alphaToken, "retain", map[string]any{
		"items": []any{map[string]any{"content": "second write"}},
	}); isErr {
		t.Fatal("second retain failed")
	}
	after := countPrefix(s.fake.requestPaths(), "PUT /v1/default/banks/ops")
	if after != before {
		t.Fatalf("bank creation was repeated: %d -> %d PUTs", before, after)
	}
}

// --- Upstream credential hygiene ----------------------------------------------

func TestCallerTokenIsNeverForwardedUpstream(t *testing.T) {
	s := newTestServer(t, routingTable)

	if _, isErr := s.callTool(alphaToken, "recall", map[string]any{"query": "anything"}); isErr {
		t.Fatal("recall failed")
	}

	// The proxy must present its own management credential upstream. Agent
	// tokens are only meaningful at the proxy.
	for _, seen := range s.fake.authHeaders() {
		if seen == "Bearer "+alphaToken {
			t.Fatal("the caller's token was forwarded to Hindsight")
		}
	}
	if !contains(s.fake.authHeaders(), "Bearer "+upstreamToken) {
		t.Fatalf("upstream never saw the management token: %v", s.fake.authHeaders())
	}
}

// A path that is not the MCP endpoint must not reveal that this is a proxy.
func TestUnknownPathIsNotFound(t *testing.T) {
	s := newTestServer(t, routingTable)

	resp := s.callRaw(alphaToken, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if resp.status != http.StatusOK {
		t.Fatalf("sanity: /mcp should work, got %d", resp.status)
	}

	req, _ := http.NewRequest(http.MethodPost, s.proxy.URL+"/mcp/ops/", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+alphaToken)
	req.Header.Set("Content-Type", "application/json")
	out, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer out.Body.Close()
	if out.StatusCode != http.StatusNotFound {
		t.Fatalf("bank-in-path status = %d, want 404", out.StatusCode)
	}
}

// --- Helpers ------------------------------------------------------------------

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}

func countPrefix(paths []string, prefix string) int {
	n := 0
	for _, p := range paths {
		if strings.HasPrefix(p, prefix) {
			n++
		}
	}
	return n
}

// decodeItems is a convenience for reading a JSON tool result.
func decodeItems(t *testing.T, text string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(text), v); err != nil {
		t.Fatalf("decode tool result %q: %v", text, err)
	}
}

var _ = config.ScopeOwn
