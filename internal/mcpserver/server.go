// Package mcpserver serves the MCP endpoint. Every request is authenticated,
// resolved to a routing rule, and then given a tool list scoped to that rule.
package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzttzzmyswy/hindsight-proxy/internal/config"
	"github.com/zzttzzmyswy/hindsight-proxy/internal/upstream"
)

// ServerName is the implementation name reported in the MCP handshake.
const ServerName = "hindsight-proxy"

// Options configures the MCP server.
type Options struct {
	Config  *config.Manager
	Client  *upstream.Client
	Logger  *slog.Logger
	Version string
}

// Server authenticates callers and serves each one a tool list scoped to its
// routing rule.
type Server struct {
	cfg     *config.Manager
	client  *upstream.Client
	logger  *slog.Logger
	version string

	handler http.Handler
	// streamable dispatches to the per-caller MCP server returned by serverFor.
	streamable *mcp.StreamableHTTPHandler
	// servers caches one MCP server per distinct (bank, tool set) pair. It is a
	// cache and not a routing table: a config reload that changes a token's
	// tools simply produces a different key, and the stale entry stops being
	// looked up.
	servers sync.Map
}

// New builds the MCP server.
func New(o Options) *Server {
	s := &Server{
		cfg:     o.Config,
		client:  o.Client,
		logger:  o.Logger,
		version: o.Version,
	}
	s.streamable = mcp.NewStreamableHTTPHandler(s.serverFor, &mcp.StreamableHTTPOptions{
		// Stateless: an agent run is a short-lived process that may issue a
		// single tools/call. There is no session to resume and therefore no
		// cross-run state to leak between tokens.
		Stateless: true,
		// JSON responses: with no sessions there is nothing to stream, and a
		// plain JSON body is the response shape every MCP client handles.
		JSONResponse: true,
		Logger:       o.Logger,
	})
	return s
}

// Handler serves the MCP endpoint.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	// Every response on this endpoint is specific to the caller's token.
	w.Header().Set("Cache-Control", "no-store")

	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		unauthorized(w, "missing Authorization: Bearer <token>")
		return
	}
	// Resolved per request from the live table, so a reload takes effect with no
	// restart and no cached decision outliving the token's rule.
	rule, ok := s.cfg.Current().Lookup(token)
	if !ok {
		// Fail closed: an unknown token never falls back to a default bank.
		s.logger.Warn("rejected unknown token",
			"remote", r.RemoteAddr, "token", redactToken(token), "path", r.URL.Path)
		unauthorized(w, "unknown token")
		return
	}
	if r.URL.Path != "/mcp" {
		// Bank selection is the proxy's job; an agent must not be able to name a
		// bank in the path the way it can against Hindsight directly. Anything
		// else on this listener is a 404, not a 401, so an unauthenticated probe
		// cannot map the endpoint layout.
		notFound(w)
		return
	}
	s.streamable.ServeHTTP(w, r.WithContext(withRule(r.Context(), rule)))
}

// serverFor returns the MCP server bound to this caller's bank and tool set.
func (s *Server) serverFor(r *http.Request) *mcp.Server {
	rule, ok := ruleFrom(r.Context())
	if !ok {
		// Unreachable in practice: serve() has already authenticated. Returning
		// a tool-less server keeps a bug here from becoming an open door.
		return s.buildServer(config.Rule{Tools: config.ToolList{}}, ruleKey{})
	}
	return s.buildServer(rule, ruleKeyOf(rule))
}

// ctxKey types the context values this package stores.
type ctxKey struct{ name string }

var ruleCtxKey = ctxKey{"rule"}

// withRule parks the resolved rule on the request context. The rule travels
// with the request instead of being re-derived from a header, because the
// streamable transport does not guarantee the header survives every internal
// re-dispatch of a single POST.
func withRule(ctx context.Context, rule config.Rule) context.Context {
	return context.WithValue(ctx, ruleCtxKey, rule)
}

func ruleFrom(ctx context.Context) (config.Rule, bool) {
	if ctx == nil {
		return config.Rule{}, false
	}
	rule, ok := ctx.Value(ruleCtxKey).(config.Rule)
	return rule, ok
}

// ruleKey identifies a cached MCP server. The agent is part of the key because
// the server's instructions name the caller's own ownership tag, so two tokens
// sharing a bank and tool set but acting as different agents need distinct
// servers.
type ruleKey struct {
	Bank  string
	Agent string
	Tools string
}

func ruleKeyOf(rule config.Rule) ruleKey {
	return ruleKey{Bank: rule.Bank, Agent: rule.Agent, Tools: toolSignature(rule.Tools)}
}

// toolSignature is a stable string for a token's tool selection.
func toolSignature(t config.ToolList) string {
	if t.All() {
		return config.WildcardTools
	}
	names := append([]string(nil), t.Names()...)
	sort.Strings(names)
	return strings.Join(names, ",")
}

func (s *Server) buildServer(rule config.Rule, key ruleKey) *mcp.Server {
	if v, ok := s.servers.Load(key); ok {
		return v.(*mcp.Server)
	}
	ns := mcp.NewServer(&mcp.Implementation{
		Name:    ServerName,
		Version: s.version,
		Title:   "Hindsight memory proxy",
	}, &mcp.ServerOptions{
		Instructions: instructionsFor(rule),
		Logger:       s.logger,
	})

	for _, def := range Registry {
		if !rule.Tools.Allowed(def.Name) {
			continue
		}
		name := def.Name
		ns.AddTool(&mcp.Tool{
			Name:        def.Name,
			Description: def.Description,
			InputSchema: def.Schema,
			Annotations: def.Annotations,
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// The rule is re-read from the context rather than captured, so a
			// tool call is always evaluated against the routing table that is
			// live now, even on a server instance built before a reload.
			cur, ok := ruleFrom(ctx)
			if !ok {
				return toolError("not authenticated")
			}
			args, err := callArgs(req)
			if err != nil {
				return toolError("invalid arguments: %v", err)
			}
			return s.call(ctx, cur, name, args)
		})
	}

	actual, _ := s.servers.LoadOrStore(key, ns)
	return actual.(*mcp.Server)
}

// instructionsFor describes the caller's own routing, so an agent knows which
// bank it is reading and writing and whether its reads are scoped.
func instructionsFor(rule config.Rule) string {
	var b strings.Builder
	b.WriteString("Memory access through the Hindsight proxy. ")
	b.WriteString("Your requests are routed to bank ")
	b.WriteString(rule.Bank)
	b.WriteString("; the bank is not a tool parameter.")
	if rule.OwnsReports() {
		b.WriteString(" Reads are scoped to entries you wrote.")
	} else if rule.Agent != "" {
		b.WriteString(" Reads cover the whole bank.")
	}
	if rule.Agent != "" {
		b.WriteString(" Entries you write are stamped ")
		b.WriteString(rule.OwnTag())
		b.WriteString(" by the proxy; do not send agent: tags yourself.")
	}
	// The usage protocol ships with the handshake rather than with each agent's
	// own instructions: one place to maintain, and it applies to every caller
	// already pointed at this proxy. Instructions do not count against the tool
	// surface budget.
	b.WriteString(" ")
	b.WriteString(usageProtocol)
	return b.String()
}

// usageProtocol tells an agent when to reach for the memory tools. It is fixed
// text, identical for every caller: what varies per caller is the routing above,
// not the protocol.
const usageProtocol = "Usage: call recall with the task topic before starting work. " +
	"Before finishing, retain facts worth knowing next time: user preferences, " +
	"decisions and their reasons, environment facts (hosts, paths, versions), and " +
	"pitfalls with their fixes. Do not retain transient progress, secrets, or tokens. " +
	"Give facts that can change a stable document_id (e.g. \"env:nas-hindsight-port\") " +
	"and reuse it when the fact changes."

// ToolSurfaceSize is the serialized size in characters of the full tool surface,
// used by the health endpoint to keep the surface within its budget.
func ToolSurfaceSize() int { return toolSurfaceSize(sdkTools()) }

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="hindsight-proxy"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
}

// redactToken keeps logs free of usable credentials while still distinguishing
// one rejected token from another.
func redactToken(token string) string {
	if len(token) <= 6 {
		return "***"
	}
	return token[:3] + "***" + token[len(token)-2:]
}
