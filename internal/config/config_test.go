package config

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testLogger keeps reload chatter out of the test output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// known reports the proxy's real tool names, so a config typo is rejected the
// way it would be at startup.
func known(name string) bool {
	switch name {
	case "retain", "recall", "reflect", "list_memories", "get_memory", "list_tags":
		return true
	}
	return false
}

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const twoTokenConfig = `{
  "default_bank": "shared",
  "tokens": {
    "tok-ops":    {"agent": "Ops",    "bank": "ops",    "tools": ["recall", "list_tags"]},
    "tok-shared": {"agent": "Shared", "bank": "shared", "tools": "*"}
  }
}`

// Covers acceptance criterion 1: a known token routes to the bank its rule names.
func TestLookupRoutesTokenToItsBank(t *testing.T) {
	cfg, err := Load(write(t, twoTokenConfig), known)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cases := []struct {
		token string
		bank  string
		agent string
	}{
		{"tok-ops", "ops", "Ops"},
		{"tok-shared", "shared", "Shared"},
	}
	for _, tc := range cases {
		rule, ok := cfg.Lookup(tc.token)
		if !ok {
			t.Fatalf("token %q was not found", tc.token)
		}
		if rule.Bank != tc.bank {
			t.Errorf("token %q routed to bank %q, want %q", tc.token, rule.Bank, tc.bank)
		}
		if rule.Agent != tc.agent {
			t.Errorf("token %q resolved agent %q, want %q", tc.token, rule.Agent, tc.agent)
		}
	}
}

// Covers acceptance criterion 1: an unknown token is not resolved at all, so the
// caller is rejected rather than falling back to default_bank.
func TestLookupRejectsUnknownTokenWithoutFallingBack(t *testing.T) {
	cfg, err := Load(write(t, twoTokenConfig), known)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, token := range []string{"nope", "", "tok-op", "tok-ops ", "TOK-OPS", "shared"} {
		if rule, ok := cfg.Lookup(token); ok {
			t.Errorf("token %q resolved to bank %q; an unknown token must not resolve", token, rule.Bank)
		}
	}
}

func TestLookupDistinguishesTokensThatDifferOnlyInSuffix(t *testing.T) {
	// A prefix-sharing token must not match a configured one.
	body := `{"default_bank":"b","tokens":{"abcdef":  {"agent":"A","tools":"*"},
	                                       "abcdefg": {"agent":"B","tools":"*"}}}`
	cfg, err := Load(write(t, body), known)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rule, ok := cfg.Lookup("abcdefg")
	if !ok || rule.Agent != "B" {
		t.Fatalf("abcdefg resolved to %+v (ok=%v), want agent B", rule, ok)
	}
}

func TestBankFallsBackToDefaultBank(t *testing.T) {
	body := `{"default_bank":"shared","tokens":{"t1":{"agent":"A","tools":"*"}}}`
	cfg, err := Load(write(t, body), known)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rule, _ := cfg.Lookup("t1")
	if rule.Bank != "shared" {
		t.Fatalf("bank = %q, want the default_bank", rule.Bank)
	}
}

func TestLoadRejectsBadConfigs(t *testing.T) {
	cases := map[string]string{
		"no tokens":           `{"default_bank":"b","tokens":{}}`,
		"missing bank":        `{"tokens":{"t":{"agent":"A","tools":"*"}}}`,
		"bad bank id":         `{"default_bank":"has space","tokens":{"t":{"agent":"A","tools":"*"}}}`,
		"bank with a slash":   `{"default_bank":"a/b","tokens":{"t":{"agent":"A","tools":"*"}}}`,
		"unknown tool":        `{"default_bank":"b","tokens":{"t":{"agent":"A","tools":["recall","drop_table"]}}}`,
		"empty tools list":    `{"default_bank":"b","tokens":{"t":{"agent":"A","tools":[]}}}`,
		"tools as bad string": `{"default_bank":"b","tokens":{"t":{"agent":"A","tools":"some"}}}`,
		"bad read_scope":      `{"default_bank":"b","tokens":{"t":{"agent":"A","tools":"*","read_scope":"mine"}}}`,
		"own without agent":   `{"default_bank":"b","tokens":{"t":{"tools":"*","read_scope":"own"}}}`,
		"unknown field":       `{"default_bank":"b","tokens":{"t":{"agent":"A","tools":"*","scope":"own"}}}`,
		"empty token key":     `{"default_bank":"b","tokens":{"  ":{"agent":"A","tools":"*"}}}`,
		"not json":            `{`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body), known); err == nil {
				t.Fatalf("Load accepted an invalid config (%s)", name)
			}
		})
	}
}

func TestOwnScopeRequiresAnAgentToAttributeReadsTo(t *testing.T) {
	body := `{"default_bank":"b","tokens":{"t":{"agent":"A","tools":"*","read_scope":"own"}}}`
	cfg, err := Load(write(t, body), known)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rule, _ := cfg.Lookup("t")
	if !rule.OwnsReports() {
		t.Fatal("read_scope own did not mark the rule as own-scoped")
	}
	if rule.OwnTag() != "agent:A" {
		t.Fatalf("OwnTag = %q, want agent:A", rule.OwnTag())
	}
}

func TestReadScopeDefaultsToShared(t *testing.T) {
	cfg, err := Load(write(t, twoTokenConfig), known)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rule, _ := cfg.Lookup("tok-shared")
	if rule.OwnsReports() {
		t.Fatal("read_scope defaulted to own; it must default to shared")
	}
}

func TestToolListAllocatedByRule(t *testing.T) {
	cfg, err := Load(write(t, twoTokenConfig), known)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ops, _ := cfg.Lookup("tok-ops")
	if ops.Tools.Allowed("retain") {
		t.Error("tok-ops allows retain; its rule lists only recall and list_tags")
	}
	if !ops.Tools.Allowed("recall") || !ops.Tools.Allowed("list_tags") {
		t.Error("tok-ops must allow the two tools its rule names")
	}
	shared, _ := cfg.Lookup("tok-shared")
	for _, name := range []string{"retain", "recall", "reflect", "list_memories", "get_memory", "list_tags"} {
		if !shared.Tools.Allowed(name) {
			t.Errorf(`tools "*" must allow %q`, name)
		}
	}
}

// Covers acceptance criterion 5: editing the file takes effect without a restart.
func TestManagerReloadsOnFileChange(t *testing.T) {
	path := write(t, `{"default_bank":"b","tokens":{"t1":{"agent":"A","tools":"*"}}}`)
	mgr, err := NewManager(path, known, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, ok := mgr.Current().Lookup("t2"); ok {
		t.Fatal("t2 resolved before it was configured")
	}

	// Watch must be running before the edit, since the baseline fingerprint is
	// taken from the file as it stands when Watch starts.
	stop := make(chan struct{})
	defer close(stop)
	go mgr.Watch(5*time.Millisecond, stop)

	// Rewrite with an added token and a changed bank for t1.
	if err := os.WriteFile(path, []byte(`{"default_bank":"b","tokens":{
		"t1":{"agent":"A","bank":"moved","tools":["recall"]},
		"t2":{"agent":"B","tools":"*"}}}`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rule, ok := mgr.Current().Lookup("t2")
		if ok && rule.Bank == "b" {
			r1, _ := mgr.Current().Lookup("t1")
			if r1.Bank != "moved" {
				t.Fatalf("t1 bank = %q after reload, want moved", r1.Bank)
			}
			if r1.Tools.Allowed("reflect") {
				t.Fatal("t1 tool list was not narrowed by the reload")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the added token never took effect; reload did not happen")
}

// A bad edit must not knock out a working routing table.
func TestManagerKeepsServingAfterABadEdit(t *testing.T) {
	path := write(t, `{"default_bank":"b","tokens":{"t1":{"agent":"A","tools":"*"}}}`)
	mgr, err := NewManager(path, known, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if err := os.WriteFile(path, []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := mgr.Reload(); err == nil {
		t.Fatal("Reload accepted a table with no tokens")
	}
	if _, ok := mgr.Current().Lookup("t1"); !ok {
		t.Fatal("the previous routing table was lost after a failed reload")
	}
}

// Reload must adopt a valid edit.
func TestReloadAdoptsAValidEdit(t *testing.T) {
	path := write(t, `{"default_bank":"b","tokens":{"t1":{"agent":"A","tools":"*"}}}`)
	mgr, err := NewManager(path, known, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"default_bank":"c","tokens":{"t2":{"agent":"B","tools":"*"}}}`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := mgr.Current().Lookup("t2"); !ok {
		t.Fatal("the reloaded table was not adopted")
	}
	if _, ok := mgr.Current().Lookup("t1"); ok {
		t.Fatal("a token removed by the edit is still routable")
	}
}

func TestToolListUnmarshalRejectsAnObject(t *testing.T) {
	var tl ToolList
	if err := tl.UnmarshalJSON([]byte(`{"names":["recall"]}`)); err == nil {
		t.Fatal("ToolList accepted an object")
	}
	var round ToolList
	if err := round.UnmarshalJSON([]byte(`"*"`)); err != nil {
		t.Fatalf(`ToolList rejected "*": %v`, err)
	}
	if !round.All() {
		t.Fatal(`"*" did not marshal to the wildcard form`)
	}
}

func TestRedactNeverLeaksTheToken(t *testing.T) {
	for _, tok := range []string{"short", "a-very-long-secret-token", "ab"} {
		got := redact(tok)
		if len(tok) > 6 && strings.Contains(got, tok) {
			t.Fatalf("redact(%q) = %q leaks the token", tok, got)
		}
	}
}
