// Command hindsight-proxy serves a token-routed MCP endpoint in front of the
// Hindsight HTTP API. A caller presents a bearer token; the proxy resolves it to
// a bank, injects the caller's ownership tag on writes, and trims the tool list.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zzttzzmyswy/hindsight-proxy/internal/config"
	"github.com/zzttzzmyswy/hindsight-proxy/internal/mcpserver"
	"github.com/zzttzzmyswy/hindsight-proxy/internal/upstream"
)

// version is the single version source for this service; it is stamped at build
// time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

type settings struct {
	addr            string
	configPath      string
	upstreamURL     string
	upstreamToken   string
	reloadInterval  time.Duration
	requestTimeout  time.Duration
	shutdownTimeout time.Duration
	logLevel        string
	printSurface    bool
	healthcheck     bool
	healthcheckURL  string
}

func run() error {
	fs := flag.NewFlagSet("hindsight-proxy", flag.ContinueOnError)
	var s settings
	fs.StringVar(&s.addr, "listen", envOr("LISTEN_ADDR", ":8890"), "address to serve the MCP endpoint on")
	fs.StringVar(&s.configPath, "config", envOr("CONFIG_PATH", "/etc/hindsight-proxy/tokens.json"),
		"path to the token -> bank routing table (polled and hot-reloaded)")
	fs.StringVar(&s.upstreamURL, "upstream", envOr("HINDSIGHT_URL", "http://127.0.0.1:8888"),
		"base URL of the Hindsight API")
	fs.StringVar(&s.upstreamToken, "upstream-token", envOr("HINDSIGHT_TOKEN", ""),
		"management token for Hindsight; required, never taken from a caller")
	fs.DurationVar(&s.reloadInterval, "reload-interval", 2*time.Second,
		"how often to poll the routing table for changes")
	fs.DurationVar(&s.requestTimeout, "request-timeout", 120*time.Second,
		"overall timeout for one upstream request")
	fs.DurationVar(&s.shutdownTimeout, "shutdown-timeout", 15*time.Second,
		"how long to let in-flight requests finish on shutdown")
	fs.StringVar(&s.logLevel, "log-level", envOr("LOG_LEVEL", "info"), "debug, info, warn or error")
	fs.BoolVar(&s.printSurface, "print-tool-surface-size", false,
		"print the serialized size of the full tool surface and exit")
	fs.BoolVar(&s.healthcheck, "healthcheck", false,
		"probe the local /healthz once and exit non-zero when it is unhealthy")
	fs.StringVar(&s.healthcheckURL, "healthcheck-url", envOr("HEALTHCHECK_URL", "http://127.0.0.1:8890/healthz"),
		"URL the -healthcheck probe requests")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	if s.printSurface {
		fmt.Println(mcpserver.ToolSurfaceSize())
		return nil
	}
	if s.healthcheck {
		return probeHealth(s.healthcheckURL)
	}
	if s.healthcheck {
		return probeHealth(s.healthcheckURL)
	}

	logger, err := newLogger(s.logLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	if strings.TrimSpace(s.upstreamToken) == "" {
		return errors.New("upstream token is required (set HINDSIGHT_TOKEN or -upstream-token); " +
			"without it the proxy cannot authenticate to Hindsight")
	}

	// Config first: a bad routing table must stop the process rather than leave
	// it running with no rules, which would reject every token and look like an
	// authentication outage.
	cfgMgr, err := config.NewManager(s.configPath, mcpserver.KnownTool, logger)
	if err != nil {
		return fmt.Errorf("load routing table: %w", err)
	}
	for _, r := range cfgMgr.Current().Rules() {
		logger.Info("routing rule loaded",
			"bank", r.Bank, "agent", r.Agent, "read_scope", r.ReadScope,
			"tools", toolSummary(r.Tools), "description", r.Description)
	}
	logger.Info("tool surface", "tools", len(mcpserver.Registry), "serialized_chars", mcpserver.ToolSurfaceSize())

	client, err := upstream.New(upstream.Options{
		BaseURL: s.upstreamURL,
		Token:   s.upstreamToken,
		Timeout: s.requestTimeout,
		Logger:  logger,
	})
	if err != nil {
		return fmt.Errorf("build upstream client: %w", err)
	}

	// Probe the upstream once at boot so an unusable token or unreachable host
	// is reported here instead of on the first agent call.
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 10*time.Second)
	err = client.Ping(probeCtx)
	cancelProbe()
	if err != nil {
		logger.Warn("upstream probe failed; serving anyway and retrying per request",
			"upstream", s.upstreamURL, "error", err)
	} else {
		logger.Info("upstream reachable", "upstream", s.upstreamURL)
	}

	mcpSrv := mcpserver.New(mcpserver.Options{
		Config:  cfgMgr,
		Client:  client,
		Logger:  logger,
		Version: version,
	})

	stop := make(chan struct{})
	go cfgMgr.Watch(s.reloadInterval, stop)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpSrv.Handler())
	mux.Handle("/mcp/", mcpSrv.Handler())
	mux.HandleFunc("/healthz", healthHandler(client, cfgMgr))

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: reflect and high-budget recall legitimately run for
		// minutes, and a write deadline would cut them off mid-answer.
		IdleTimeout: 120 * time.Second,
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("serving", "addr", s.addr, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		close(stop)
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		close(stop)
		shutCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

// healthHandler reports liveness plus the two facts an operator needs when a
// caller reports trouble: is the upstream reachable, and how many rules are live.
func healthHandler(client *upstream.Client, cfg *config.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		status := http.StatusOK
		upErr := client.Ping(ctx)
		if upErr != nil {
			status = http.StatusServiceUnavailable
		}
		w.WriteHeader(status)
		up := "ok"
		if upErr != nil {
			up = upErr.Error()
		}
		fmt.Fprintf(w, "{\"status\":%q,\"upstream\":%q,\"rules\":%d}\n",
			map[bool]string{true: "ok", false: "degraded"}[upErr == nil], up, len(cfg.Current().Rules()))
	}
}

// probeHealth backs the container healthcheck. The image has no shell and no
// curl, so the binary checks itself.
func probeHealth(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("health probe: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health probe: status %d", resp.StatusCode)
	}
	return nil
}

func newLogger(level string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", level, err)
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func toolSummary(t config.ToolList) string {
	if t.All() {
		return config.WildcardTools
	}
	return strings.Join(t.Names(), ",")
}
