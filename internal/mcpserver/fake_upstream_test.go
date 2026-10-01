package mcpserver_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
)

// fakeHindsight stands in for the Hindsight HTTP API. It records every request
// the proxy sends, so tests can assert on what actually crossed the boundary
// rather than on what the proxy returned.
type fakeHindsight struct {
	mu sync.Mutex

	// banks maps bank id to its stored memory items.
	banks map[string][]storedMemory
	// created records bank ids passed to PUT /banks/{id}.
	created map[string]bool
	// writes records the retain bodies the proxy sent, per bank.
	writes map[string][]retainBody
	// calls records "METHOD path" for every request.
	calls []string
	// auths records the Authorization header of every request, so a test can
	// prove the caller's token never reaches upstream.
	auths []string
	// bankIDs records the bank each request resolved to, in call order.
	bankIDs []string
	// requireAuth rejects requests without the expected bearer token.
	requireAuth string
}

type storedMemory struct {
	ID   string   `json:"id"`
	Text string   `json:"text"`
	Type string   `json:"fact_type"`
	Tags []string `json:"tags"`
}

type retainBody struct {
	Items []struct {
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
		Context string   `json:"context"`
	} `json:"items"`
	Async bool `json:"async"`
}

func newFakeHindsight(upstreamToken string) *fakeHindsight {
	return &fakeHindsight{
		banks:       map[string][]storedMemory{},
		created:     map[string]bool{},
		writes:      map[string][]retainBody{},
		requireAuth: upstreamToken,
	}
}

func (f *fakeHindsight) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.requireAuth != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != f.requireAuth {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"Authentication failed: Invalid or missing API key"}`))
			return
		}
	}

	body, _ := io.ReadAll(r.Body)
	bank, suffix := splitBankPath(r.URL.Path)

	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.bankIDs = append(f.bankIDs, bank)
	_, bankExists := f.banks[bank]
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodPut && suffix == "":
		f.mu.Lock()
		if !bankExists {
			f.banks[bank] = nil
		}
		f.created[bank] = true
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"bank_id": bank, "name": bank})

	case r.Method == http.MethodPost && suffix == "/memories":
		if !bankExists {
			notFound(w)
			return
		}
		var in retainBody
		if err := json.Unmarshal(body, &in); err != nil {
			writeJSON(w, 422, map[string]any{"detail": err.Error()})
			return
		}
		f.mu.Lock()
		f.writes[bank] = append(f.writes[bank], in)
		for i, it := range in.Items {
			f.banks[bank] = append(f.banks[bank], storedMemory{
				ID:   idFor(bank, len(f.banks[bank])+i),
				Text: it.Content,
				Type: "world",
				Tags: it.Tags,
			})
		}
		n := len(in.Items)
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{
			"success": true, "bank_id": bank, "items_count": n, "async": in.Async,
		})

	case r.Method == http.MethodPost && suffix == "/memories/recall":
		if !bankExists {
			notFound(w)
			return
		}
		var in struct {
			Query string   `json:"query"`
			Tags  []string `json:"tags"`
		}
		_ = json.Unmarshal(body, &in)
		f.mu.Lock()
		results := []map[string]any{}
		for _, m := range f.banks[bank] {
			if !tagFilterAllows(in.Tags, m.Tags) {
				continue
			}
			results = append(results, map[string]any{
				"id": m.ID, "text": m.Text, "type": m.Type, "tags": m.Tags,
			})
		}
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"results": results})

	case r.Method == http.MethodPost && suffix == "/reflect":
		if !bankExists {
			notFound(w)
			return
		}
		var in struct {
			Tags []string `json:"tags"`
		}
		_ = json.Unmarshal(body, &in)
		f.mu.Lock()
		memories := []map[string]any{}
		for _, m := range f.banks[bank] {
			if !tagFilterAllows(in.Tags, m.Tags) {
				continue
			}
			memories = append(memories, map[string]any{
				"id": m.ID, "text": m.Text, "type": m.Type, "tags": m.Tags,
			})
		}
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{
			"text":     "synthesised",
			"based_on": map[string]any{"memories": memories, "mental_models": []any{}, "directives": []any{}},
		})

	case r.Method == http.MethodGet && suffix == "/memories/list":
		if !bankExists {
			notFound(w)
			return
		}
		f.mu.Lock()
		items := append([]storedMemory(nil), f.banks[bank]...)
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{
			"items": items, "total": len(items), "limit": 50, "offset": 0,
		})

	case r.Method == http.MethodGet && strings.HasPrefix(suffix, "/memories/"):
		if !bankExists {
			notFound(w)
			return
		}
		want := strings.TrimPrefix(suffix, "/memories/")
		f.mu.Lock()
		var found *storedMemory
		for i := range f.banks[bank] {
			if f.banks[bank][i].ID == want {
				m := f.banks[bank][i]
				found = &m
				break
			}
		}
		f.mu.Unlock()
		if found == nil {
			notFound(w)
			return
		}
		writeJSON(w, 200, found)

	case r.Method == http.MethodGet && suffix == "/tags":
		if !bankExists {
			notFound(w)
			return
		}
		f.mu.Lock()
		counts := map[string]int{}
		for _, m := range f.banks[bank] {
			for _, t := range m.Tags {
				counts[t]++
			}
		}
		f.mu.Unlock()
		names := make([]string, 0, len(counts))
		for t := range counts {
			names = append(names, t)
		}
		sort.Strings(names)
		items := make([]map[string]any, 0, len(names))
		for _, t := range names {
			items = append(items, map[string]any{"tag": t, "count": counts[t]})
		}
		writeJSON(w, 200, map[string]any{
			"items": items, "total": len(items), "limit": 100, "offset": 0,
		})

	case r.Method == http.MethodGet && r.URL.Path == "/v1/default/banks":
		names := []string{}
		f.mu.Lock()
		for b := range f.banks {
			names = append(names, b)
		}
		f.mu.Unlock()
		sort.Strings(names)
		out := make([]map[string]any, 0, len(names))
		for _, b := range names {
			out = append(out, map[string]any{"bank_id": b, "name": b})
		}
		writeJSON(w, 200, map[string]any{"items": out, "total": len(out)})

	default:
		notFound(w)
	}
}

// accountFor reads the recorded state under the lock.
func (f *fakeHindsight) accountFor(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeHindsight) tokensWrittenTo(bank string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := [][]string{}
	for _, w := range f.writes[bank] {
		for _, it := range w.Items {
			out = append(out, it.Tags)
		}
	}
	return out
}

func (f *fakeHindsight) requestPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeHindsight) start() *httptest.Server { return httptest.NewServer(f) }

// splitBankPath pulls the bank id out of /v1/default/banks/{bank}/<suffix>.
func splitBankPath(p string) (bank, suffix string) {
	const prefix = "/v1/default/banks/"
	if !strings.HasPrefix(p, prefix) {
		return "", p
	}
	rest := strings.TrimPrefix(p, prefix)
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i], rest[i:]
	}
	return rest, ""
}

// tagFilterAllows mirrors upstream's default "any" matching: no filter means
// everything, and a filter matches a memory carrying any requested tag OR
// carrying no tags at all. Modelling the untagged case matters: it is precisely
// how a memory the caller does not own can slip past a request-side tag filter,
// which is why own-scope is enforced on the response too.
func tagFilterAllows(filter, tags []string) bool {
	if len(filter) == 0 {
		return true
	}
	if len(tags) == 0 {
		return true
	}
	for _, want := range filter {
		for _, got := range tags {
			if want == got {
				return true
			}
		}
	}
	return false
}

func idFor(bank string, n int) string {
	return bank + "-mem-" + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"detail": "bank not found"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
