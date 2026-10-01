package upstream

import (
	"net/url"
	"strconv"
)

// MemoryItem is one memory to store. Field names and shapes are the subset of
// Hindsight's MemoryItem that this proxy's retain tool exposes; tags are filled
// in by the proxy, never by the caller.
type MemoryItem struct {
	Content    any               `json:"content"`
	Context    string            `json:"context,omitempty"`
	Timestamp  string            `json:"timestamp,omitempty"`
	Tags       []string          `json:"tags,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	DocumentID string            `json:"document_id,omitempty"`
}

// RetainRequest is the body of POST /memories.
type RetainRequest struct {
	Items []MemoryItem `json:"items"`
	// Async stays false: the proxy's retain tool promises a synchronous write.
	Async bool `json:"async"`
}

// RetainResponse is what POST /memories returns.
type RetainResponse struct {
	Success    bool   `json:"success"`
	BankID     string `json:"bank_id"`
	ItemsCount int    `json:"items_count"`
	Async      bool   `json:"async"`
}

// RecallRequest is the body of POST /memories/recall. Only the fields this
// proxy's recall tool exposes are declared; the rest of Hindsight's request
// surface stays unreachable.
type RecallRequest struct {
	Query     string   `json:"query"`
	Budget    string   `json:"budget,omitempty"`
	MaxTokens int      `json:"max_tokens,omitempty"`
	Types     []string `json:"types,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	TagsMatch string   `json:"tags_match,omitempty"`
}

// RecallResponse carries the ranked results.
type RecallResponse struct {
	Results []RecallResult `json:"results"`
}

// RecallResult is one recalled fact. Only the fields worth handing to an agent
// are decoded; the rest of the upstream payload is dropped rather than
// forwarded, which keeps a recall result small.
type RecallResult struct {
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	Type        string   `json:"type,omitempty"`
	Context     string   `json:"context,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	DocumentID  string   `json:"document_id,omitempty"`
	MentionedAt string   `json:"mentioned_at,omitempty"`
	Scores      any      `json:"scores,omitempty"`
}

// ReflectRequest is the body of POST /reflect.
type ReflectRequest struct {
	Query     string   `json:"query"`
	Budget    string   `json:"budget,omitempty"`
	MaxTokens int      `json:"max_tokens,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	TagsMatch string   `json:"tags_match,omitempty"`
}

// ReflectResponse is the synthesised answer.
type ReflectResponse struct {
	Text    string          `json:"text"`
	BasedOn *ReflectBasedOn `json:"based_on,omitempty"`
}

// ReflectBasedOn is the evidence the answer was built from. Only the memory
// facts are read: under an "own" read scope they are the part that can leak
// another caller's content, and a mental model or directive belongs to the bank
// rather than to one caller.
type ReflectBasedOn struct {
	Memories []ReflectFact `json:"memories"`
}

// ReflectFact is one piece of evidence behind a reflection.
type ReflectFact struct {
	ID   string   `json:"id"`
	Text string   `json:"text"`
	Type string   `json:"type,omitempty"`
	Tags []string `json:"tags"`
}

// ListMemoriesQuery are the query parameters of GET /memories/list.
type ListMemoriesQuery struct {
	Type      string
	Q         string
	Limit     int
	Offset    int
	Tags      []string
	TagsMatch string
}

// ListMemoriesResponse is a page of memories.
type ListMemoriesResponse struct {
	Items  []MemoryListItem `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

// MemoryListItem is one row of a browse page.
type MemoryListItem struct {
	ID       string         `json:"id"`
	Text     string         `json:"text"`
	Context  string         `json:"context,omitempty"`
	Date     string         `json:"date,omitempty"`
	FactType string         `json:"fact_type,omitempty"`
	Tags     []string       `json:"tags"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// ListTagsQuery are the query parameters of GET /tags.
type ListTagsQuery struct {
	Q     string
	Limit int
}

// ListTagsResponse is a page of tags with counts.
type ListTagsResponse struct {
	Items  []TagItem `json:"items"`
	Total  int       `json:"total"`
	Limit  int       `json:"limit"`
	Offset int       `json:"offset"`
}

// TagItem is one tag and how many memories carry it.
type TagItem struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// Encode renders the list-memories query, omitting unset fields so the upstream
// default applies rather than a zero value being sent explicitly.
func (q ListMemoriesQuery) Encode() string {
	var limit, offset string
	if q.Limit > 0 {
		limit = strconv.Itoa(q.Limit)
	}
	if q.Offset > 0 {
		offset = strconv.Itoa(q.Offset)
	}
	return encodeQuery([][2]string{
		{"type", q.Type},
		{"q", q.Q},
		{"limit", limit},
		{"offset", offset},
		{"tags_match", q.TagsMatch},
	})
}

// Encode renders the list-tags query.
func (q ListTagsQuery) Encode() string {
	var limit string
	if q.Limit > 0 {
		limit = strconv.Itoa(q.Limit)
	}
	return encodeQuery([][2]string{
		{"q", q.Q},
		{"limit", limit},
	})
}

// encodeQuery renders a query string, skipping zero values.
func encodeQuery(pairs [][2]string) string {
	q := url.Values{}
	for _, p := range pairs {
		if p[1] != "" {
			q.Set(p[0], p[1])
		}
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}
