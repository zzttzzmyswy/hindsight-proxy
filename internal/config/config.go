// Package config loads and hot-reloads the token → bank routing table that
// decides which Hindsight bank a caller may reach.
package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// Read scopes for a token rule.
const (
	// ScopeShared leaves reads unfiltered: the caller sees the whole bank.
	ScopeShared = "shared"
	// ScopeOwn restricts every read to memories carrying the caller's own
	// ownership tag.
	ScopeOwn = "own"
)

// WildcardTools means "do not trim this token's tool list".
const WildcardTools = "*"

// bankIDPattern keeps a bank id safe to interpolate into a URL path.
var bankIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// agentTagPattern mirrors the ownership tag shape injected on write.
var agentTagPattern = regexp.MustCompile(`^agent:[^\s,]+$`)

// File is the on-disk shape of the routing table.
type File struct {
	// DefaultBank is used only for token entries that omit "bank". An unknown
	// token never falls back to it.
	DefaultBank string                `json:"default_bank"`
	Tokens      map[string]TokenEntry `json:"tokens"`
}

// TokenEntry is one caller's rule.
type TokenEntry struct {
	// Agent is the name injected as the `agent:<name>` ownership tag. When
	// empty the caller writes no ownership tag and may not use read_scope
	// "own".
	Agent string `json:"agent"`
	// Bank is the target bank id. Falls back to File.DefaultBank.
	Bank string `json:"bank"`
	// Tools is either "*" or an explicit allow-list of tool names.
	Tools ToolList `json:"tools"`
	// ReadScope is "shared" (default) or "own".
	ReadScope string `json:"read_scope"`
	// Description is documentation only.
	Description string `json:"description"`
}

// ToolList unmarshals from either "*" or an array of tool names.
type ToolList struct {
	all   bool
	names []string
}

func (t *ToolList) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if s != WildcardTools {
			return fmt.Errorf("tools must be %q or an array of tool names, got %q", WildcardTools, s)
		}
		t.all, t.names = true, nil
		return nil
	}
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		return fmt.Errorf("tools must be %q or an array of tool names: %w", WildcardTools, err)
	}
	t.all, t.names = false, names
	return nil
}

// MarshalJSON round-trips ToolList back to its on-disk form.
func (t ToolList) MarshalJSON() ([]byte, error) {
	if t.all {
		return json.Marshal(WildcardTools)
	}
	return json.Marshal(t.names)
}

// All reports whether every tool is allowed.
func (t ToolList) All() bool { return t.all }

// Allowed reports whether name is in the list. Wildcard allows everything.
func (t ToolList) Allowed(name string) bool {
	if t.all {
		return true
	}
	for _, n := range t.names {
		if n == name {
			return true
		}
	}
	return false
}

// Names returns the explicit names, or nil when wildcard.
func (t ToolList) Names() []string { return t.names }

// Rule is a validated token rule, ready to serve a request.
type Rule struct {
	Agent       string
	Bank        string
	ReadScope   string
	Tools       ToolList
	Description string

	// tokenHash is the SHA-256 of the configured token, so lookups compare
	// fixed-width digests instead of short-circuiting on length.
	tokenHash [sha256.Size]byte
}

// OwnsReports reports whether this rule scopes reads to its own tag.
func (r Rule) OwnsReports() bool { return r.ReadScope == ScopeOwn }

// OwnTag is the injected ownership tag, or "" when the rule has no agent.
func (r Rule) OwnTag() string {
	if r.Agent == "" {
		return ""
	}
	return "agent:" + r.Agent
}

// Config is an immutable, validated routing snapshot.
type Config struct {
	DefaultBank string
	rules       []Rule
}

// Rules returns the validated rules in load order.
func (c *Config) Rules() []Rule { return c.rules }

// Load reads and validates the routing table at path. knownTool reports whether
// a name from a token's "tools" allow-list is a tool this proxy actually serves,
// so a typo fails the reload instead of silently producing an empty tool set.
func Load(path string, knownTool func(string) bool) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return f.build(path, knownTool)
}

func (f *File) build(path string, knownTool func(string) bool) (*Config, error) {
	if len(f.Tokens) == 0 {
		return nil, fmt.Errorf("%s: no tokens configured; refusing to start fail-open", path)
	}
	if f.DefaultBank != "" && !bankIDPattern.MatchString(f.DefaultBank) {
		return nil, fmt.Errorf("%s: default_bank %q is not a valid bank id", path, f.DefaultBank)
	}

	rules := make([]Rule, 0, len(f.Tokens))
	seenBank := map[string]string{}
	for token, e := range f.Tokens {
		if strings.TrimSpace(token) == "" {
			return nil, fmt.Errorf("%s: empty token key", path)
		}
		bank := e.Bank
		if bank == "" {
			bank = f.DefaultBank
		}
		if bank == "" {
			return nil, fmt.Errorf("%s: token %s has no bank and no default_bank is set", path, redact(token))
		}
		if !bankIDPattern.MatchString(bank) {
			return nil, fmt.Errorf("%s: token %s: bank %q is not a valid bank id", path, redact(token), bank)
		}
		scope := e.ReadScope
		if scope == "" {
			scope = ScopeShared
		}
		if scope != ScopeShared && scope != ScopeOwn {
			return nil, fmt.Errorf("%s: token %s: read_scope %q must be %q or %q",
				path, redact(token), scope, ScopeShared, ScopeOwn)
		}
		if scope == ScopeOwn && e.Agent == "" {
			return nil, fmt.Errorf("%s: token %s: read_scope %q requires an agent name", path, redact(token), ScopeOwn)
		}
		if e.Agent != "" && !agentTagPattern.MatchString("agent:"+e.Agent) {
			return nil, fmt.Errorf("%s: token %s: agent name %q is not usable in an agent:<name> tag",
				path, redact(token), e.Agent)
		}
		if !e.Tools.All() {
			if len(e.Tools.Names()) == 0 {
				return nil, fmt.Errorf("%s: token %s: tools list is empty; use %q to allow every tool",
					path, redact(token), WildcardTools)
			}
			for _, n := range e.Tools.Names() {
				if !knownTool(n) {
					return nil, fmt.Errorf("%s: token %s: unknown tool %q", path, redact(token), n)
				}
			}
		}

		if other, dup := seenBank[bank]; dup && other != token {
			// Not an error: several callers may legitimately share a bank.
			slog.Debug("bank shared by multiple tokens", "bank", bank)
		}
		seenBank[bank] = token

		rules = append(rules, Rule{
			Agent:       e.Agent,
			Bank:        bank,
			ReadScope:   scope,
			Tools:       e.Tools,
			Description: e.Description,
			tokenHash:   sha256.Sum256([]byte(token)),
		})
	}
	return &Config{DefaultBank: f.DefaultBank, rules: rules}, nil
}

// Lookup resolves a bearer token to its rule. It walks every rule without
// short-circuiting so a miss costs the same as a hit.
func (c *Config) Lookup(token string) (Rule, bool) {
	sum := sha256.Sum256([]byte(token))
	var found Rule
	hit := 0
	for _, r := range c.rules {
		if subtle.ConstantTimeCompare(sum[:], r.tokenHash[:]) == 1 {
			found, hit = r, 1
		}
	}
	return found, hit == 1
}

// redact keeps logs from carrying a usable credential.
func redact(token string) string {
	if len(token) <= 6 {
		return "***"
	}
	return token[:3] + "***" + token[len(token)-2:]
}

// Manager holds the live routing snapshot and swaps it when the file changes.
type Manager struct {
	path      string
	knownTool func(string) bool
	cur       atomic.Pointer[Config]
	// last fingerprints the config content last adopted, so Watch reacts to a
	// real change rather than a timestamp touch. Only Watch reads or writes it.
	last   string
	logger *slog.Logger
}

// NewManager loads path and returns a manager serving that snapshot.
func NewManager(path string, knownTool func(string) bool, logger *slog.Logger) (*Manager, error) {
	cfg, err := Load(path, knownTool)
	if err != nil {
		return nil, err
	}
	m := &Manager{path: path, knownTool: knownTool, logger: logger}
	m.cur.Store(cfg)
	// The fingerprint is captured here, at load, not when Watch starts: a file
	// rewritten between the two would otherwise be adopted as the baseline and
	// its change never seen.
	m.last = m.stamp()
	return m, nil
}

// Current returns the snapshot serving requests right now.
func (m *Manager) Current() *Config { return m.cur.Load() }

// Reload re-reads the file. On any error the previous snapshot stays live.
func (m *Manager) Reload() error {
	cfg, err := Load(m.path, m.knownTool)
	if err != nil {
		return err
	}
	m.cur.Store(cfg)
	return nil
}

// Watch polls the file and reloads on change until ctx is done. Polling (not
// inotify) is deliberate: a config written by a rename from another container
// or over a bind mount is not reliably reported by filesystem events.
func (m *Manager) Watch(interval time.Duration, stop <-chan struct{}) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	last := m.last
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			now := m.stamp()
			if now == last {
				continue
			}
			// Advance the baseline even on a failed load: the file content at
			// `now` has been seen, and not advancing would retry it every tick
			// and bury the real error in repeated log lines.
			m.last = now
			last = now
			if err := m.Reload(); err != nil {
				m.logger.Error("config reload failed, keeping previous routing table",
					"path", m.path, "error", err)
				continue
			}
			m.logger.Info("routing table reloaded", "path", m.path, "rules", len(m.Current().Rules()))
		}
	}
}

// stamp fingerprints the config file so a reload only happens on real change.
func (m *Manager) stamp() string {
	fi, err := os.Stat(m.path)
	if err != nil {
		return "err:" + err.Error()
	}
	raw, err := os.ReadFile(m.path)
	if err != nil {
		return fmt.Sprintf("err:%v:%d", err, fi.ModTime().UnixNano())
	}
	return fmt.Sprintf("%d:%s", fi.ModTime().UnixNano(), sha256.Sum256(raw))
}
