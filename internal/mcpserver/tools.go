// Package mcpserver exposes the proxy's curated MCP tool surface and forwards
// each call to the Hindsight HTTP API.
package mcpserver

import (
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool names as agents see them. `retain` is synchronous on purpose: the
// upstream REST call waits for completion, so a write is queryable the moment
// the tool returns and agents never need a separate "sync" variant.
const (
	ToolRetain       = "retain"
	ToolRecall       = "recall"
	ToolReflect      = "reflect"
	ToolListMemories = "list_memories"
	ToolGetMemory    = "get_memory"
	ToolListTags     = "list_tags"
)

// destructiveUpstream lists Hindsight MCP tools that mutate or delete data
// irreversibly. None of them is part of this proxy's surface: even a token with
// tools:"*" cannot reach them, because the proxy never registers them.
var destructiveUpstream = []string{
	"delete_bank", "clear_memories", "delete_document",
}

// obj is shorthand for a JSON-Schema object node.
func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func strEnum(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func strArray(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

func strMap(desc string) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": desc}
}

// ToolDef is one tool this proxy serves, with the upstream REST binding.
type ToolDef struct {
	Name string
	// Description is written for this proxy, not copied from Hindsight's own
	// tool text, which is far too long to ship six of.
	Description string
	// Schema is the trimmed input schema: only the parameters an agent needs.
	Schema map[string]any
	// Annotations carries the MCP safety hints.
	Annotations *mcp.ToolAnnotations
}

var readOnly = func() *mcp.ToolAnnotations {
	openWorld := false
	return &mcp.ToolAnnotations{
		ReadOnlyHint:  true,
		OpenWorldHint: &openWorld,
	}
}

var addOnly = func() *mcp.ToolAnnotations {
	openWorld := false
	destructive := false
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    false,
		DestructiveHint: &destructive,
		OpenWorldHint:   &openWorld,
	}
}

// Registry is the ordered tool surface exposed over MCP.
//
// The schemas carry only the parameters an agent needs to do the job. Upstream
// offers a dozen more per tool (score floors, temporal windows, trace toggles,
// tag groups); each one costs description budget on every request and none is
// required to write or read a memory, so the proxy does not advertise them.
// Retain is deliberately synchronous: the upstream call waits for completion,
// so a write is queryable the moment the tool returns and there is no separate
// "sync" variant to choose between.
var Registry = []ToolDef{
	{
		Name:        ToolRetain,
		Description: "Store memories. Blocks until stored, so a later recall sees them. The proxy stamps ownership tags itself.",
		Annotations: addOnly(),
		Schema: obj(map[string]any{
			"items": map[string]any{
				"type": "array",
				"items": obj(map[string]any{
					"content":   str("The fact to remember."),
					"context":   str("Category label."),
					"timestamp": str("ISO 8601 event time."),
					"tags":      strArray("Tags for later filtering."),
				}, "content"),
			},
		}, "items"),
	},
	{
		Name:        ToolRecall,
		Description: "Search memories semantically. Use at the start of a task to recover relevant context.",
		Annotations: readOnly(),
		Schema: obj(map[string]any{
			"query":      str("Natural-language search."),
			"budget":     strEnum("Search effort.", "low", "mid", "high"),
			"max_tokens": integer("Token budget for results."),
			"types":      strArray("Fact types: world, experience, observation."),
			"tags":       strArray("Only memories with these tags."),
			"tags_match": strEnum("How tags match.", "any", "all", "any_strict", "all_strict", "exact"),
		}, "query"),
	},
	{
		Name:        ToolReflect,
		Description: "Answer a question from stored memories. Use for synthesis, not lookup.",
		Annotations: readOnly(),
		Schema: obj(map[string]any{
			"query":      str("The question to reason about."),
			"budget":     strEnum("Reasoning effort.", "low", "mid", "high"),
			"max_tokens": integer("Token budget for the answer."),
			"tags":       strArray("Scope the memories considered."),
			"tags_match": strEnum("How tags match.", "any", "all", "any_strict", "all_strict", "exact"),
		}, "query"),
	},
	{
		Name:        ToolListMemories,
		Description: "Browse memories with filters, unranked. Use to audit what is stored.",
		Annotations: readOnly(),
		Schema: obj(map[string]any{
			"type":   strEnum("Fact type filter.", "world", "experience", "observation"),
			"q":      str("Substring filter on text."),
			"limit":  integer("Page size, default 50."),
			"offset": integer("Results to skip."),
		}),
	},
	{
		Name:        ToolGetMemory,
		Description: "Fetch one memory by id, with its tags and metadata.",
		Annotations: readOnly(),
		Schema:      obj(map[string]any{"memory_id": str("Id from recall or list_memories.")}, "memory_id"),
	},
	{
		Name:        ToolListTags,
		Description: "List tags in use with counts, to discover what recall can filter on.",
		Annotations: readOnly(),
		Schema: obj(map[string]any{
			"q":     str("Glob pattern, e.g. project:*"),
			"limit": integer("Page size, default 100."),
		}),
	},
}

// KnownTool reports whether name is served by this proxy.
func KnownTool(name string) bool {
	for _, t := range Registry {
		if t.Name == name {
			return true
		}
	}
	return false
}

// sdkTools converts the registry into MCP tool definitions.
func sdkTools() []*mcp.Tool {
	out := make([]*mcp.Tool, 0, len(Registry))
	for _, t := range Registry {
		out = append(out, &mcp.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Schema,
			Annotations: t.Annotations,
		})
	}
	return out
}

// toolSurfaceSize reports the serialized size of the tools a caller sees, which
// is what the "< 3000 characters" budget in the issue measures.
func toolSurfaceSize(tools []*mcp.Tool) int {
	total := 0
	for _, t := range tools {
		for _, v := range []any{t.Name, t.Description, t.InputSchema} {
			b, err := json.Marshal(v)
			if err == nil {
				total += len(b)
			}
		}
	}
	return total
}
