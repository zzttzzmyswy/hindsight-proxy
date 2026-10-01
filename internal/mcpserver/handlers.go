package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzttzzmyswy/hindsight-proxy/internal/config"
	"github.com/zzttzzmyswy/hindsight-proxy/internal/upstream"
)

// agentTagPrefix marks the server-injected ownership tag. Any tag a caller
// sends with this prefix is stripped before the write leaves the proxy, so the
// value in the store is always the one the routing table assigned.
const agentTagPrefix = "agent:"

// defaultListLimit and maxListLimit bound the browse endpoints. Hindsight
// applies no limit of its own on these, so the proxy imposes one: an unbounded
// listing would let a single call pull an entire bank into an agent's context.
const (
	defaultListLimit = 50
	maxListLimit     = 200
	defaultTagLimit  = 100
	maxTagLimit      = 1000
)

// argsToStrings coerces a decoded JSON array into []string.
func argsToStrings(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case []string:
		return t, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("expected an array of strings, got %T element", e)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected an array of strings, got %T", v)
	}
}

// argsToStringMap coerces a decoded JSON object into map[string]string.
func argsToStringMap(v any) (map[string]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case map[string]string:
		return t, nil
	case map[string]any:
		out := make(map[string]string, len(t))
		for k, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("metadata value for %q must be a string, got %T", k, e)
			}
			out[k] = s
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected a map of strings, got %T", v)
	}
}

// intArg reads an integer argument, tolerating the float64 that JSON decoding
// produces. A missing or null value yields 0.
func intArg(args map[string]any, name string) (int, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return 0, nil
	}
	switch t := v.(type) {
	case float64:
		if t != float64(int(t)) {
			return 0, fmt.Errorf("%s must be a whole number", name)
		}
		return int(t), nil
	case int:
		return t, nil
	case int64:
		return int(t), nil
	default:
		return 0, fmt.Errorf("%s must be a number, got %T", name, v)
	}
}

func stringArg(args map[string]any, name string) (string, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %T", name, v)
	}
	return s, nil
}

// clampLimit applies the proxy's paging bounds.
func clampLimit(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

// stripAgentTags removes every caller-supplied ownership tag. Without this a
// caller could write `agent:someone-else` and be counted as that agent by any
// reader filtering on the tag.
func stripAgentTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(t)), agentTagPrefix) {
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mergeOwnTag appends the assigned ownership tag to caller tags, replacing any
// previous agent tag (already stripped) with the authoritative one.
func mergeOwnTag(tags []string, own string) []string {
	if own == "" {
		return tags
	}
	out := make([]string, 0, len(tags)+1)
	out = append(out, tags...)
	for _, t := range out {
		if t == own {
			return out
		}
	}
	return append(out, own)
}

// scopeTags applies the rule's read scope to a caller-supplied tag filter.
// Under "own" the caller's tag filter is replaced outright rather than
// intersected: a caller must not be able to widen its own view by naming other
// agents' tags, and an "any" match over foreign tags would do exactly that.
func scopeTags(rule config.Rule, tags []string) ([]string, string, bool) {
	if !rule.OwnsReports() {
		return tags, "", false
	}
	return []string{rule.OwnTag()}, "all", true
}

// enforceOwnScope drops results that do not carry the caller's ownership tag.
//
// This is deliberately enforced on the response rather than left to the
// request-side filter. Upstream's tag matching defaults to "any (includes
// untagged)", and an observation synthesised from a caller's facts is not
// guaranteed to inherit that caller's tags -- so a request-side filter alone
// can still return another agent's derived content.
func enforceOwnScope(own string, results []upstream.RecallResult) []upstream.RecallResult {
	return filterOwn(own, results, func(r upstream.RecallResult) []string { return r.Tags })
}

func filterOwn[T any](own string, items []T, tagsOf func(T) []string) []T {
	out := make([]T, 0, len(items))
	for _, it := range items {
		for _, t := range tagsOf(it) {
			if t == own {
				out = append(out, it)
				break
			}
		}
	}
	return out
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// jsonResult renders v as pretty JSON text content. Results are returned as
// text, not structured content, because the MCP structured-output contract
// requires an outputSchema that a pass-through proxy cannot declare faithfully.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}, nil
}

// toolError reports a caller-side failure. IsError is set so the agent sees the
// message as a tool result it can react to, rather than a transport fault.
func toolError(format string, a ...any) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, a...)}},
		IsError: true,
	}, nil
}

// callArgs decodes the raw arguments object. The SDK hands the handler the
// original JSON body, and a client calling a parameterless tool may omit it
// entirely or send an empty object.
func callArgs(req *mcp.CallToolRequest) (map[string]any, error) {
	if req == nil || req.Params == nil || len(req.Params.Arguments) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(req.Params.Arguments, &m); err != nil {
		return nil, fmt.Errorf("decode arguments: %w", err)
	}
	if m == nil {
		return map[string]any{}, nil
	}
	return m, nil
}

// call runs one tool call and maps every failure the caller can act on into a
// tool error, so an agent sees a message it can reason about instead of a
// transport fault. It also logs the upstream status for operators.
//
// The target bank is created on demand first: Hindsight answers 404 for every
// endpoint of a bank that does not exist, so without this a caller routed to a
// fresh bank would fail until an operator created it by hand.
func (s *Server) call(ctx context.Context, rule config.Rule, name string, args map[string]any) (*mcp.CallToolResult, error) {
	if err := s.client.EnsureBank(ctx, rule.Bank); err != nil {
		return toolErrorFor(s, rule, name, err)
	}
	res, err := s.invoke(ctx, rule, name, args)
	if err != nil && isBankMissing(err) {
		// The bank was deleted behind this process, so the cached "exists" is
		// stale. Re-create once and retry rather than failing the caller.
		s.client.ForgetBank(rule.Bank)
		if ensureErr := s.client.EnsureBank(ctx, rule.Bank); ensureErr == nil {
			res, err = s.invoke(ctx, rule, name, args)
		}
	}
	if err != nil {
		return toolErrorFor(s, rule, name, err)
	}
	return res, nil
}

// isBankMissing reports whether an upstream failure means the bank is gone.
func isBankMissing(err error) bool {
	var se *upstream.StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

// invoke dispatches to the tool implementation for the resolved rule.
func (s *Server) invoke(ctx context.Context, rule config.Rule, name string, args map[string]any) (*mcp.CallToolResult, error) {
	var (
		res *mcp.CallToolResult
		err error
	)
	switch name {
	case ToolRetain:
		res, err = s.handleRetain(ctx, rule, args)
	case ToolRecall:
		res, err = s.handleRecall(ctx, rule, args)
	case ToolReflect:
		res, err = s.handleReflect(ctx, rule, args)
	case ToolListMemories:
		res, err = s.handleListMemories(ctx, rule, args)
	case ToolGetMemory:
		res, err = s.handleGetMemory(ctx, rule, args)
	case ToolListTags:
		res, err = s.handleListTags(ctx, rule, args)
	default:
		// Unreachable: the tool was registered from Registry, and a token can
		// only reach tools its rule allows.
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	return res, err
}

// toolErrorFor turns an upstream failure into the message the agent sees.
func toolErrorFor(s *Server, rule config.Rule, tool string, err error) (*mcp.CallToolResult, error) {
	s.logUpstreamFailure(rule, tool, err)
	var se *upstream.StatusError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusNotFound:
			return toolError("the requested item does not exist in your memory bank")
		case http.StatusUnauthorized, http.StatusForbidden:
			return toolError("the proxy's upstream credentials were rejected; this is an operator configuration problem")
		case http.StatusTooManyRequests:
			return toolError("upstream is rate limiting; retry shortly")
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return toolError("upstream rejected the request: %s", se.Body)
		}
		if se.Status >= 500 {
			return toolError("memory service is unavailable (upstream %d)", se.Status)
		}
		return toolError("upstream error %d: %s", se.Status, se.Body)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return toolError("memory service timed out")
	}
	return toolError("memory service request failed: %v", err)
}

func (s *Server) handleRetain(ctx context.Context, rule config.Rule, args map[string]any) (*mcp.CallToolResult, error) {
	rawList, ok := args["items"].([]any)
	if !ok || len(rawList) == 0 {
		return toolError("items is required and must be a non-empty array")
	}

	own := rule.OwnTag()
	items := make([]upstream.MemoryItem, 0, len(rawList))
	for i, raw := range rawList {
		m, ok := raw.(map[string]any)
		if !ok {
			return toolError("items[%d] must be an object", i)
		}
		content, err := stringArg(m, "content")
		if err != nil {
			return toolError("items[%d].content: %v", i, err)
		}
		if strings.TrimSpace(content) == "" {
			return toolError("items[%d].content is required", i)
		}
		tags, err := argsToStrings(m["tags"])
		if err != nil {
			return toolError("items[%d].tags: %v", i, err)
		}
		metadata, err := argsToStringMap(m["metadata"])
		if err != nil {
			return toolError("items[%d].metadata: %v", i, err)
		}
		timestamp, err := stringArg(m, "timestamp")
		if err != nil {
			return toolError("items[%d].timestamp: %v", i, err)
		}
		contextLabel, err := stringArg(m, "context")
		if err != nil {
			return toolError("items[%d].context: %v", i, err)
		}
		documentID, err := stringArg(m, "document_id")
		if err != nil {
			return toolError("items[%d].document_id: %v", i, err)
		}

		if stripped := stripAgentTags(tags); len(stripped) != len(tags) {
			s.logger.Info("stripped caller-supplied agent tags before write",
				"bank", rule.Bank, "agent", rule.Agent, "dropped", len(tags)-len(stripped))
		}

		items = append(items, upstream.MemoryItem{
			Content: content,
			Context: contextLabel,
			// "unset" is Hindsight's sentinel for a timeless fact; an empty
			// string is not equivalent, and omitting the field would stamp
			// the memory with the current time.
			Timestamp:  timestamp,
			Tags:       mergeOwnTag(stripAgentTags(tags), own),
			Metadata:   metadata,
			DocumentID: documentID,
		})
	}

	resp, err := s.client.Retain(ctx, rule.Bank, upstream.RetainRequest{Items: items})
	if err != nil {
		return nil, err
	}
	s.logger.Info("retained", "bank", rule.Bank, "agent", rule.Agent, "items", len(items))
	return jsonResult(resp)
}

func (s *Server) handleRecall(ctx context.Context, rule config.Rule, args map[string]any) (*mcp.CallToolResult, error) {
	query, err := stringArg(args, "query")
	if err != nil {
		return toolError("query: %v", err)
	}
	if strings.TrimSpace(query) == "" {
		return toolError("query is required")
	}
	budget, err := stringArg(args, "budget")
	if err != nil {
		return toolError("budget: %v", err)
	}
	types, err := argsToStrings(args["types"])
	if err != nil {
		return toolError("types: %v", err)
	}
	tags, err := argsToStrings(args["tags"])
	if err != nil {
		return toolError("tags: %v", err)
	}
	tagsMatch, err := stringArg(args, "tags_match")
	if err != nil {
		return toolError("tags_match: %v", err)
	}
	maxTokens, err := intArg(args, "max_tokens")
	if err != nil {
		return toolError("max_tokens: %v", err)
	}

	tags, tagsMatch, scoped := scopeTags(rule, tags)
	if !scoped {
		tags = stripAgentTags(tags)
	}

	resp, err := s.client.Recall(ctx, rule.Bank, upstream.RecallRequest{
		Query:     query,
		Budget:    budget,
		MaxTokens: maxTokens,
		Types:     types,
		Tags:      tags,
		TagsMatch: tagsMatch,
	})
	if err != nil {
		return nil, err
	}

	if rule.OwnsReports() {
		before := len(resp.Results)
		resp.Results = enforceOwnScope(rule.OwnTag(), resp.Results)
		if dropped := before - len(resp.Results); dropped > 0 {
			s.logger.Debug("own-scope filtered recall results",
				"bank", rule.Bank, "agent", rule.Agent, "dropped", dropped)
		}
	}
	return jsonResult(resp)
}

func (s *Server) handleReflect(ctx context.Context, rule config.Rule, args map[string]any) (*mcp.CallToolResult, error) {
	query, err := stringArg(args, "query")
	if err != nil {
		return toolError("query: %v", err)
	}
	if strings.TrimSpace(query) == "" {
		return toolError("query is required")
	}
	budget, err := stringArg(args, "budget")
	if err != nil {
		return toolError("budget: %v", err)
	}
	tags, err := argsToStrings(args["tags"])
	if err != nil {
		return toolError("tags: %v", err)
	}
	tagsMatch, err := stringArg(args, "tags_match")
	if err != nil {
		return toolError("tags_match: %v", err)
	}
	maxTokens, err := intArg(args, "max_tokens")
	if err != nil {
		return toolError("max_tokens: %v", err)
	}

	tags, tagsMatch, scoped := scopeTags(rule, tags)
	if !scoped {
		tags = stripAgentTags(tags)
	}

	resp, err := s.client.Reflect(ctx, rule.Bank, upstream.ReflectRequest{
		Query:     query,
		Budget:    budget,
		MaxTokens: maxTokens,
		Tags:      tags,
		TagsMatch: tagsMatch,
	})
	if err != nil {
		return nil, err
	}

	// Under "own" the answer itself may cite another caller's memories, so the
	// evidence list has to be filtered too; the text cannot be.
	if rule.OwnsReports() && resp.BasedOn != nil {
		own := rule.OwnTag()
		resp.BasedOn.Memories = filterOwn(own, resp.BasedOn.Memories, func(f upstream.ReflectFact) []string { return f.Tags })
	}
	return jsonResult(resp)
}

func (s *Server) handleListMemories(ctx context.Context, rule config.Rule, args map[string]any) (*mcp.CallToolResult, error) {
	factType, err := stringArg(args, "type")
	if err != nil {
		return toolError("type: %v", err)
	}
	q, err := stringArg(args, "q")
	if err != nil {
		return toolError("q: %v", err)
	}
	limit, err := intArg(args, "limit")
	if err != nil {
		return toolError("limit: %v", err)
	}
	offset, err := intArg(args, "offset")
	if err != nil {
		return toolError("offset: %v", err)
	}
	if offset < 0 {
		return toolError("offset must not be negative")
	}

	query := upstream.ListMemoriesQuery{
		Type:   factType,
		Q:      q,
		Limit:  clampLimit(limit, defaultListLimit, maxListLimit),
		Offset: offset,
	}
	if rule.OwnsReports() {
		// Push the scope upstream as well as filtering the response. Upstream's
		// "any" matching also returns untagged memories, so this alone is not
		// sufficient -- but it keeps the page full of the caller's own entries
		// instead of entries that the response filter is about to drop, which
		// would otherwise make paging lose rows.
		query.Tags = []string{rule.OwnTag()}
		query.TagsMatch = "all"
	}

	resp, err := s.client.ListMemories(ctx, rule.Bank, query)
	if err != nil {
		return nil, err
	}

	if rule.OwnsReports() {
		own := rule.OwnTag()
		resp.Items = filterOwn(own, resp.Items, func(m upstream.MemoryListItem) []string { return m.Tags })
		// total would otherwise report the bank-wide count, contradicting items.
		resp.Total = len(resp.Items)
	}
	return jsonResult(resp)
}

func (s *Server) handleGetMemory(ctx context.Context, rule config.Rule, args map[string]any) (*mcp.CallToolResult, error) {
	memoryID, err := stringArg(args, "memory_id")
	if err != nil {
		return toolError("memory_id: %v", err)
	}
	if strings.TrimSpace(memoryID) == "" {
		return toolError("memory_id is required")
	}

	raw, err := s.client.GetMemory(ctx, rule.Bank, memoryID)
	if err != nil {
		return nil, err
	}

	if rule.OwnsReports() {
		var probe struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("decode memory: %w", err)
		}
		if !hasTag(probe.Tags, rule.OwnTag()) {
			// Report absence rather than existence: under "own" another
			// caller's memory must be indistinguishable from a missing one.
			return toolError("memory %s not found", memoryID)
		}
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: prettyJSON(raw)}},
	}, nil
}

func (s *Server) handleListTags(ctx context.Context, rule config.Rule, args map[string]any) (*mcp.CallToolResult, error) {
	q, err := stringArg(args, "q")
	if err != nil {
		return toolError("q: %v", err)
	}
	limit, err := intArg(args, "limit")
	if err != nil {
		return toolError("limit: %v", err)
	}

	resp, err := s.client.ListTags(ctx, rule.Bank, upstream.ListTagsQuery{
		Q:     q,
		Limit: clampLimit(limit, defaultTagLimit, maxTagLimit),
	})
	if err != nil {
		return nil, err
	}

	if rule.OwnsReports() {
		own := rule.OwnTag()
		resp.Items = filterOwn(own, resp.Items, func(t upstream.TagItem) []string {
			if t.Tag == own {
				return []string{own}
			}
			return nil
		})
		resp.Total = len(resp.Items)
	}
	return jsonResult(resp)
}

// prettyJSON renders raw upstream JSON for display, falling back to the raw
// bytes when it is not an object or array.
func prettyJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// logUpstreamFailure records the upstream status for operators while the caller
// sees only the message the tool handler chose.
func (s *Server) logUpstreamFailure(rule config.Rule, tool string, err error) {
	var se *upstream.StatusError
	switch {
	case errors.As(err, &se):
		s.logger.Error("upstream rejected request",
			"tool", tool, "bank", rule.Bank, "status", se.Status, "body", se.Body)
	default:
		s.logger.Error("upstream request failed", "tool", tool, "bank", rule.Bank, "error", err)
	}
}
