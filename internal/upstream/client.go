// Package upstream talks to the Hindsight HTTP API. Every request is built from
// a validated bank id and fixed endpoint suffixes, so a caller-supplied value
// never reaches the URL path.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// maxErrorBody bounds how much of an upstream error body is surfaced back.
const maxErrorBody = 2 << 10

// maxResponseBody caps what the proxy will read from upstream. Recall and
// reflect can return megabytes; this keeps a runaway response from exhausting
// the container's memory allowance.
const maxResponseBody = 32 << 20

// Client is a thin, concurrency-safe Hindsight REST client.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	logger  *slog.Logger

	// knownBanks caches banks this process has created or confirmed, so the
	// common path issues one request instead of two.
	mu         sync.RWMutex
	knownBanks map[string]struct{}
}

// Options configures a Client.
type Options struct {
	BaseURL      string
	Token        string
	Timeout      time.Duration
	MaxIdleConns int
	Logger       *slog.Logger
}

// New builds a client. The base URL must be an absolute http(s) URL with no
// query or fragment, so endpoint paths can be appended safely.
func New(o Options) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(o.BaseURL))
	if err != nil {
		return nil, fmt.Errorf("upstream base url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("upstream base url must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("upstream base url %q has no host", o.BaseURL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("upstream base url must not carry a query or fragment")
	}
	if strings.TrimSpace(o.Token) == "" {
		return nil, fmt.Errorf("upstream token is required; Hindsight must be authenticated for the proxy to be the only entry point")
	}
	if o.Timeout <= 0 {
		o.Timeout = 120 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	maxIdle := o.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = 32
	}
	tr := &http.Transport{
		MaxIdleConns:        maxIdle,
		MaxIdleConnsPerHost: maxIdle,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		baseURL: strings.TrimSuffix(u.String(), "/"),
		token:   o.Token,
		http:    &http.Client{Transport: tr, Timeout: o.Timeout},
		logger:  o.Logger,
		// A bank the proxy created stays known for the life of the process.
		// Hindsight never drops a bank on its own, so the cache cannot go stale
		// in a way that breaks a request.
		knownBanks: map[string]struct{}{},
	}, nil
}

// StatusError is an error carrying the upstream HTTP status, so the MCP layer
// can tell a caller's mistake from an upstream outage.
type StatusError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *StatusError) Error() string {
	body := strings.TrimSpace(e.Body)
	if body == "" {
		return fmt.Sprintf("hindsight %s %s: HTTP %d", e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("hindsight %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, body)
}

// BankPath builds the bank-scoped path prefix. bankID has already been checked
// against the config's id pattern, and PathEscape is applied anyway so no value
// can introduce a path separator or traversal.
func BankPath(bankID string) string {
	return "/v1/default/banks/" + url.PathEscape(bankID)
}

// do performs an upstream request, decoding a JSON response into out when out is
// non-nil and the status is 2xx.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return &StatusError{Status: resp.StatusCode, Method: method, Path: path, Body: string(raw)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody))
	if err := dec.Decode(out); err != nil {
		if err == io.EOF {
			return nil
		}
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	return nil
}

// EnsureBank creates the bank when this process has not already confirmed it
// exists. Upstream treats PUT as create-or-update, so a concurrent first call
// from the same bank is harmless.
func (c *Client) EnsureBank(ctx context.Context, bankID string) error {
	c.mu.RLock()
	_, ok := c.knownBanks[bankID]
	c.mu.RUnlock()
	if ok {
		return nil
	}

	body := map[string]any{"name": bankID}
	if err := c.do(ctx, http.MethodPut, BankPath(bankID), body, nil); err != nil {
		return fmt.Errorf("ensure bank %q: %w", bankID, err)
	}

	c.mu.Lock()
	c.knownBanks[bankID] = struct{}{}
	c.mu.Unlock()
	c.logger.Info("created or confirmed bank", "bank", bankID)
	return nil
}

// ForgetBank drops a bank from the cache so the next request re-creates it.
func (c *Client) ForgetBank(bankID string) {
	c.mu.Lock()
	delete(c.knownBanks, bankID)
	c.mu.Unlock()
}

// Ping reports whether the upstream API answers and whether it accepts this
// proxy's credential. It backs the proxy's own readiness endpoint, so a
// deployment can tell "proxy up, upstream unreachable" from "proxy up".
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/version", nil, nil)
}

// Retain stores memories. upstream waits for completion, so the write is
// queryable as soon as this returns.
func (c *Client) Retain(ctx context.Context, bankID string, req RetainRequest) (*RetainResponse, error) {
	req.Async = false
	var out RetainResponse
	if err := c.do(ctx, http.MethodPost, BankPath(bankID)+"/memories", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Recall runs semantic search.
func (c *Client) Recall(ctx context.Context, bankID string, req RecallRequest) (*RecallResponse, error) {
	var out RecallResponse
	if err := c.do(ctx, http.MethodPost, BankPath(bankID)+"/memories/recall", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Reflect synthesises an answer from stored memories.
func (c *Client) Reflect(ctx context.Context, bankID string, req ReflectRequest) (*ReflectResponse, error) {
	var out ReflectResponse
	if err := c.do(ctx, http.MethodPost, BankPath(bankID)+"/reflect", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListMemories browses memories with non-semantic filters.
func (c *Client) ListMemories(ctx context.Context, bankID string, q ListMemoriesQuery) (*ListMemoriesResponse, error) {
	path := BankPath(bankID) + "/memories/list" + q.Encode()
	var out ListMemoriesResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetMemory fetches one memory unit by id.
func (c *Client) GetMemory(ctx context.Context, bankID, memoryID string) (json.RawMessage, error) {
	path := BankPath(bankID) + "/memories/" + url.PathEscape(memoryID)
	var out json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListTags returns tags in use with their counts.
func (c *Client) ListTags(ctx context.Context, bankID string, q ListTagsQuery) (*ListTagsResponse, error) {
	path := BankPath(bankID) + "/tags" + q.Encode()
	var out ListTagsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
