package mcpserver_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzttzzmyswy/hindsight-proxy/internal/config"
	"github.com/zzttzzmyswy/hindsight-proxy/internal/mcpserver"
	"github.com/zzttzzmyswy/hindsight-proxy/internal/upstream"
)

const (
	upstreamToken = "management-token"
	alphaToken    = "token-alpha"
	betaToken     = "token-beta"
)

// testServer is one proxy wired to a fake Hindsight, with the routing table on
// disk so a test can rewrite it to exercise hot reload.
type testServer struct {
	t        *testing.T
	proxy    *httptest.Server
	fake     *fakeHindsight
	cfgPath  string
	proxyMux http.Handler
}

func newTestServer(t *testing.T, routingTable string) *testServer {
	t.Helper()
	fake := newFakeHindsight(upstreamToken)
	upstreamSrv := fake.start()
	t.Cleanup(upstreamSrv.Close)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(cfgPath, []byte(routingTable), 0o600); err != nil {
		t.Fatalf("write routing table: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := config.NewManager(cfgPath, mcpserver.KnownTool, logger)
	if err != nil {
		t.Fatalf("config.NewManager: %v", err)
	}
	client, err := upstream.New(upstream.Options{
		BaseURL: upstreamSrv.URL,
		Token:   upstreamToken,
		Timeout: 10 * time.Second,
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}

	srv := mcpserver.New(mcpserver.Options{
		Config:  mgr,
		Client:  client,
		Logger:  logger,
		Version: "test",
	})
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go mgr.Watch(time.Millisecond, stop)

	return &testServer{t: t, proxy: proxy, fake: fake, cfgPath: cfgPath, proxyMux: srv.Handler()}
}

// call issues one MCP request and returns the decoded JSON-RPC response.
func (s *testServer) call(token, method string, params any) *rpcResponse {
	s.t.Helper()
	return s.callRaw(token, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
}

func (s *testServer) callRaw(token string, payload any) *rpcResponse {
	s.t.Helper()
	auth := ""
	if token != "" {
		auth = "Bearer " + token
	}
	return s.callWithAuthHeader(auth, payload)
}

// callWithAuthHeader sends the Authorization header verbatim, so a test can
// probe header shapes a bearer-token helper would never produce.
func (s *testServer) callWithAuthHeader(auth string, payload any) *rpcResponse {
	s.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		s.t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, s.proxy.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		s.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var out rpcResponse
	out.status = resp.StatusCode
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			out.raw = string(raw)
		}
	}
	return &out
}

// toolNames lists the tools the caller can see.
func (s *testServer) toolNames(token string) []string {
	s.t.Helper()
	resp := s.call(token, "tools/list", map[string]any{})
	if resp.status != http.StatusOK {
		s.t.Fatalf("tools/list status = %d (%s)", resp.status, resp.raw)
	}
	var res struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	resp.decodeResult(s.t, &res)
	names := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// callTool invokes one tool and returns its text content.
func (s *testServer) callTool(token, name string, args map[string]any) (string, bool) {
	s.t.Helper()
	resp := s.call(token, "tools/call", map[string]any{
		"name": name, "arguments": args,
	})
	if resp.status != http.StatusOK {
		s.t.Fatalf("tools/call %s status = %d (%s)", name, resp.status, resp.raw)
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	resp.decodeResult(s.t, &res)
	if len(res.Content) == 0 {
		return "", res.IsError
	}
	return res.Content[0].Text, res.IsError
}

type rpcResponse struct {
	status int
	raw    string
	body   struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
}

// decodeResult unmarshals the JSON-RPC result into v.
func (r *rpcResponse) decodeResult(t *testing.T, v any) {
	t.Helper()
	if r.body.Error != nil {
		t.Fatalf("JSON-RPC error %d: %s", r.body.Error.Code, r.body.Error.Message)
	}
	if len(r.body.Result) == 0 {
		t.Fatalf("no result in response (status %d, body %q)", r.status, r.raw)
	}
	if err := json.Unmarshal(r.body.Result, v); err != nil {
		t.Fatalf("decode result: %v (raw %s)", err, r.body.Result)
	}
}

// waitFor polls until cond holds, so a hot-reload assertion does not depend on
// a fixed sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func sortedEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sortStrings(x)
	sortStrings(y)
	return strings.Join(x, ",") == strings.Join(y, ",")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

var _ = fmt.Sprintf

// writeFileAtomically replaces path the way a deployment would: write a sibling
// temp file, then rename over the target. A reader never observes a partial
// file, which is what makes reload-on-change safe under concurrent writes.
func writeFileAtomically(path, body string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
