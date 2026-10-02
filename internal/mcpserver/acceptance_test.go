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
// to its own writes, and a caller with a trimmed tool list.
//
// The no-agent caller sits on a bank of its own. It cannot share one: the loader
// rejects a bank named by two rules unless both name an agent, because an agent
// is what document_id is namespaced under.
const routingTable = `{
  "default_bank": "shared",
  "tokens": {
    "token-alpha":  {"agent": "Mika",   "bank": "ops",    "tools": "*"},
    "token-beta":   {"agent": "Riven",  "bank": "shared", "tools": "*"},
    "token-own":    {"agent": "Solo",   "bank": "shared", "read_scope": "own", "tools": "*"},
    "token-legacy": {"bank": "legacy", "tools": "*"},
    "token-trim":   {"agent": "Narrow", "bank": "shared", "tools": ["recall", "list_tags"]},
    "token-multi":  {"agent": "Multi",  "bank": "shared", "tools": "*"}
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
	// A memory nothing in this proxy wrote: no ownership tag at all. Seeded
	// straight into the store because the routing table can no longer express
	// it -- a bank named by two rules must name an agent on each, so there is
	// no configured caller left that would write untagged. Memories like this
	// still exist in a bank that predates the rule (or was written around the
	// proxy), and upstream's "any" tag matching returns them for any filter,
	// which is why own-scope has to filter the response rather than trust the
	// request.
	s.fake.seedMemory("shared", "untagged legacy note", nil)

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
	// Seeded rather than written through the proxy, for the reason given in the
	// recall test above: no configured caller writes untagged any more.
	s.fake.seedMemory("shared", "untagged browseable note", nil)

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

// --- The memory usage protocol reaches the agent over the handshake ------------

// The protocol cannot live in each agent's own instructions: the agents already
// pointed at this proxy carry no Hindsight guidance of their own, so a tool they
// are never told to call stays unused. It ships in the MCP handshake instead,
// which every caller receives, and must not displace the per-caller routing
// text it is appended to.
func TestInitializeInstructionsCarryRoutingAndUsageProtocol(t *testing.T) {
	s := newTestServer(t, routingTable)

	resp := s.call(alphaToken, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	var res struct {
		Instructions string `json:"instructions"`
	}
	resp.decodeResult(t, &res)

	if !strings.Contains(res.Instructions, "bank ops") {
		t.Fatalf("instructions no longer name the caller's bank: %q", res.Instructions)
	}
	if !strings.Contains(res.Instructions, "Usage: call recall") {
		t.Fatalf("instructions carry no usage protocol: %q", res.Instructions)
	}
	// Appended, not substituted: the routing sentence still comes first, and a
	// single space separates the two.
	if strings.Index(res.Instructions, "bank ops") > strings.Index(res.Instructions, "Usage: call recall") {
		t.Fatalf("the usage protocol was prepended to the routing text: %q", res.Instructions)
	}
	if strings.Contains(res.Instructions, "  ") || strings.Contains(res.Instructions, ".\n") {
		t.Fatalf("the appended paragraph is not separated by a single space: %q", res.Instructions)
	}
}

// --- retain carries document_id so an agent can correct a fact in place -------

// Reusing a document_id replaces the earlier version upstream, which is the only
// way an agent can fix a fact that has changed: the proxy exposes no delete
// tool, by design, so an outdated fact would otherwise accumulate beside its
// replacement forever.
//
// The id reaches upstream namespaced under the caller's agent. That is what lets
// the upsert be used on a shared bank: without the prefix, two agents agreeing on
// a key would replace each other's documents.
func TestRetainPassesDocumentIDUpstream(t *testing.T) {
	s := newTestServer(t, routingTable)

	if text, isErr := s.callTool(alphaToken, "retain", map[string]any{
		"items": []any{map[string]any{
			"content":     "the hindsight port is 8891",
			"document_id": "env:nas-hindsight-port",
		}},
	}); isErr {
		t.Fatalf("retain failed: %s", text)
	}

	writes := s.fake.writesTo("ops")
	if len(writes) != 1 || len(writes[0].Items) != 1 {
		t.Fatalf("upstream saw %d writes, want 1 with one item", len(writes))
	}
	got := writes[0].Items[0].DocumentID
	if got == nil {
		t.Fatal("document_id was dropped before the request reached upstream")
	}
	// Namespaced under the caller's agent: the id the caller asked for is kept
	// as the tail, so the same key written by another agent is a different
	// document rather than a replacement.
	if *got != "Mika/env:nas-hindsight-port" {
		t.Fatalf("upstream received document_id %q, want %q", *got, "Mika/env:nas-hindsight-port")
	}
}

// A caller that sends no document_id must not have one invented for it: an
// empty string is a different instruction from an absent field, and a proxy that
// sent one would collapse every untagged write onto a single document.
func TestRetainOmitsDocumentIDWhenNotGiven(t *testing.T) {
	s := newTestServer(t, routingTable)

	if text, isErr := s.callTool(alphaToken, "retain", map[string]any{
		"items": []any{map[string]any{"content": "a fact with no stable key"}},
	}); isErr {
		t.Fatalf("retain failed: %s", text)
	}

	writes := s.fake.writesTo("ops")
	if len(writes) != 1 || len(writes[0].Items) != 1 {
		t.Fatalf("upstream saw %d writes, want 1 with one item", len(writes))
	}
	if got := writes[0].Items[0].DocumentID; got != nil {
		t.Fatalf("document_id was sent without being asked for: %q", *got)
	}
}

// The parameter is only usable if an agent can discover it: a handler that
// honours document_id while the schema hides it changes nothing for the caller.
func TestRetainSchemaAdvertisesDocumentID(t *testing.T) {
	var retain *mcpserver.ToolDef
	for i := range mcpserver.Registry {
		if mcpserver.Registry[i].Name == mcpserver.ToolRetain {
			retain = &mcpserver.Registry[i]
			break
		}
	}
	if retain == nil {
		t.Fatal("retain is not in the registry")
	}

	items, _ := retain.Schema["properties"].(map[string]any)["items"].(map[string]any)
	props, _ := items["items"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["document_id"]; !ok {
		t.Fatalf("retain's item schema does not advertise document_id: %v", props)
	}

	// Advertised over MCP too, not just in the Go value: the schema an agent
	// reads is the one that goes out in tools/list.
	s := newTestServer(t, routingTable)
	resp := s.call(alphaToken, "tools/list", map[string]any{})
	var res struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	resp.decodeResult(t, &res)
	for _, tl := range res.Tools {
		if tl.Name != mcpserver.ToolRetain {
			continue
		}
		schema := mustJSON(t, tl.InputSchema)
		if !strings.Contains(schema, `"document_id"`) {
			t.Fatalf("tools/list schema for retain has no document_id: %s", schema)
		}
		return
	}
	t.Fatal("retain is missing from tools/list")
}

// --- document_id is namespaced under the writing agent -------------------------

// Two agents on one bank writing the same document_id must produce two distinct
// documents, each namespaced under its own writer. This is the whole point of
// the change: before it, the second write replaced the first.
//
// The bank is shared and both callers are on it, which is the configuration the
// loader demands an agent for.
func TestTwoAgentsSharingABankDoNotCollideOnDocumentID(t *testing.T) {
	s := newTestServer(t, routingTable)

	const key = "env:shared-port"
	for _, w := range []struct{ token, content string }{
		{betaToken, "the shared port is 8891"},
		{"token-multi", "the shared port is 8892"},
	} {
		if text, isErr := s.callTool(w.token, "retain", map[string]any{
			"items": []any{map[string]any{"content": w.content, "document_id": key}},
		}); isErr {
			t.Fatalf("retain as %s failed: %s", w.token, text)
		}
	}

	writes := s.fake.writesTo("shared")
	if len(writes) != 2 {
		t.Fatalf("upstream saw %d writes, want 2", len(writes))
	}
	first, second := writes[0].Items[0].DocumentID, writes[1].Items[0].DocumentID
	if first == nil || second == nil {
		t.Fatalf("a document_id was dropped: %v, %v", first, second)
	}
	if *first == *second {
		t.Fatalf("both agents wrote document_id %q; the second would replace the first", *first)
	}
	// Not just different: each is its own agent's namespace plus the key the
	// caller asked for, so neither can reach the other's document.
	if *first != "Riven/"+key {
		t.Fatalf("upstream received %q, want %q", *first, "Riven/"+key)
	}
	if *second != "Multi/"+key {
		t.Fatalf("upstream received %q, want %q", *second, "Multi/"+key)
	}
}

// A caller that already holds a namespaced id -- as recall returns it -- must be
// able to write it back to correct that document rather than creating a nested
// one. Without this the correction path the upsert exists for would be
// unreachable on a namespaced id.
func TestRetainIsIdempotentOnAnAlreadyNamespacedID(t *testing.T) {
	s := newTestServer(t, routingTable)

	for _, content := range []string{"the ops port is 8891", "the ops port is 8890"} {
		if text, isErr := s.callTool(alphaToken, "retain", map[string]any{
			"items": []any{map[string]any{
				"content":     content,
				"document_id": "Mika/env:nas-hindsight-port",
			}},
		}); isErr {
			t.Fatalf("retain failed: %s", text)
		}
	}

	writes := s.fake.writesTo("ops")
	if len(writes) != 2 {
		t.Fatalf("upstream saw %d writes, want 2", len(writes))
	}
	for i, w := range writes {
		if got := w.Items[0].DocumentID; got == nil || *got != "Mika/env:nas-hindsight-port" {
			t.Fatalf("write %d carried document_id %v, want Mika/env:nas-hindsight-port", i, got)
		}
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
