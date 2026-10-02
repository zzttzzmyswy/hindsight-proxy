package mcpserver

import (
	"testing"

	"github.com/zzttzzmyswy/hindsight-proxy/internal/config"
)

// The namespace mapping in every combination the retain handler can hand it.
//
// The rule that carried the decision is checked here rather than through a call:
// the mapping is the whole of it, and a table covers the cases a request-level
// test could only reach one at a time.
func TestScopedDocumentIDNamespacesUnderTheAgent(t *testing.T) {
	mika := config.Rule{Agent: "Mika"}
	anonymous := config.Rule{}

	cases := []struct {
		name string
		rule config.Rule
		id   string
		want string
	}{
		// An absent id stays absent. The caller asked for no stable key, and
		// inventing one would collapse every keyless write onto one document.
		{"empty id stays empty", mika, "", ""},
		{"empty id with no agent stays empty", anonymous, "", ""},

		// The ordinary case.
		{"id is prefixed with the agent", mika, "env:nas-hindsight-port", "Mika/env:nas-hindsight-port"},

		// Idempotent: an id read back from recall already carries the prefix,
		// and writing it again must address that same document rather than
		// nesting it a second time.
		{"own prefix is not repeated", mika, "Mika/env:nas-hindsight-port", "Mika/env:nas-hindsight-port"},
		{"an id that merely looks prefixed is prefixed", mika, "MikaX/env:port", "Mika/MikaX/env:port"},

		// Another agent's id is not this caller's to write. The prefix goes on
		// top, so this lands on Mika/Riven/x and cannot replace Riven/x.
		{"another agent's prefix is nested", mika, "Riven/x", "Mika/Riven/x"},

		// A rule with no agent has nothing to namespace under, so the id is
		// forwarded as v0.1.4 forwarded it.
		{"no agent leaves the id alone", anonymous, "env:nas-hindsight-port", "env:nas-hindsight-port"},
		{"no agent leaves a prefixed id alone", anonymous, "Mika/x", "Mika/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scopedDocumentID(tc.rule, tc.id); got != tc.want {
				t.Fatalf("scopedDocumentID(agent %q, id %q) = %q, want %q", tc.rule.Agent, tc.id, got, tc.want)
			}
		})
	}
}
