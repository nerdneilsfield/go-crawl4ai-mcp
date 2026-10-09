package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/appconfig"
	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/server"
	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/supervisor"
)

var version = "0.1.0"

type config struct {
	addr         string
	baseURL      string
	c4aToken     string
	mcpToken     string
	searxngURL   string
	enableJS     bool
	enableAsk    bool
	production   bool
	crawlTimeout time.Duration
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(envOr(key, fmt.Sprintf("%v", def))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		// Also accept bare seconds for operator convenience (CRAWL_TIMEOUT=120).
		if secs, serr := strconv.Atoi(v); serr == nil {
			return time.Duration(secs) * time.Second
		}
		return def
	}
	return d
}

// bearerAuth rejects requests without a valid static bearer token.
// An empty token config disables auth (dev/loopback only).
func bearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		h := r.Header.Get("Authorization")
		const prefix = "bearer "
		if !strings.HasPrefix(strings.ToLower(h), prefix) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimSpace(h[len(prefix):])), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="crawl4ai-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func rootCmd() *cobra.Command {
	cfg := &config{}
	cmd := &cobra.Command{
		Use:     "crawl4ai-mcp",
		Short:   "Stateless Streamable HTTP MCP server for Crawl4AI (+ optional SearXNG web_search)",
		Version: version,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, cfg)
		},
		SilenceUsage: true,
	}
	pf := cmd.Flags()
	pf.StringVar(&cfg.addr, "addr", envOr("MCP_ADDR", ":8080"), "listen address")
	pf.StringVar(&cfg.baseURL, "crawl4ai-url", envOr("CRAWL4AI_URL", "http://127.0.0.1:11235"), "Crawl4AI server base URL")
	pf.StringVar(&cfg.c4aToken, "crawl4ai-token", os.Getenv("CRAWL4AI_TOKEN"), "Crawl4AI API token")
	pf.StringVar(&cfg.mcpToken, "mcp-token", os.Getenv("MCP_TOKEN"), "bearer token for MCP clients; empty disables auth")
	pf.StringVar(&cfg.searxngURL, "searxng-url", envOr("SEARXNG_URL", ""), "SearXNG base URL; registers web_search when set")
	pf.BoolVar(&cfg.enableJS, "enable-execute-js", envBool("ENABLE_EXECUTE_JS", false), "expose execute_js tool")
	pf.BoolVar(&cfg.enableAsk, "enable-ask", envBool("ENABLE_ASK", true), "expose ask tool")
	pf.BoolVar(&cfg.production, "log-production", envBool("LOG_PRODUCTION", false), "structured JSON logs instead of console")
	pf.DurationVar(&cfg.crawlTimeout, "crawl-timeout", envDuration("CRAWL_TIMEOUT", 90*time.Second), "per-call upstream Crawl4AI timeout; 0 disables")
	return cmd
}

func run(cmd *cobra.Command, cfg *config) error {
	flush := appconfig.Setup(cfg.production)
	defer flush()
	log := appconfig.Logger

	srv, err := server.New(server.Options{
		Crawl4AIBaseURL: strings.TrimRight(cfg.baseURL, "/"),
		Crawl4AIToken:   cfg.c4aToken,
		EnableExecuteJS: cfg.enableJS,
		EnableAsk:       cfg.enableAsk,
		SearXNGBaseURL:  strings.TrimRight(cfg.searxngURL, "/"),
	})
	if err != nil {
		return fmt.Errorf("building server: %w", err)
	}
	appconfig.CrawlTimeout = cfg.crawlTimeout
	if cfg.crawlTimeout > 0 {
		log.Info("crawl timeout enabled", zap.Duration("timeout", cfg.crawlTimeout))
	} else {
		log.Info("crawl timeout disabled (unlimited)")
	}

	// Stateless streamable HTTP: each POST is self-contained, GET/DELETE
	// return 405. JSONResponse avoids a standing SSE stream per request.
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return srv
	}, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		PropagateRequestCancellation: true, // client abort/timeout cancels in-flight upstream calls
	})

	mux := http.NewServeMux()
	mux.Handle("/mcp", bearerAuth(cfg.mcpToken, handler))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":%q,"generation":%q}`, version, envOr("RESTART_GEN", "0"))
	})

	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: tool calls proxy long browser crawls.
	}

	// Listener: inherit from a restarting parent when present, else bind.
	var (
		ln        net.Listener
		readyFile *os.File
	)
	if lFile, ready, ierr := supervisor.Inherit(); ierr != nil {
		return ierr
	} else if lFile != nil {
		defer lFile.Close()
		readyFile = ready
		if ready != nil {
			defer ready.Close()
		}
		ln, err = net.FileListener(lFile)
		if err != nil {
			return fmt.Errorf("inheriting listener: %w", err)
		}
		log.Info("inherited listener from parent", zap.String("addr", ln.Addr().String()))
	} else {
		ln, err = net.Listen("tcp", cfg.addr)
		if err != nil {
			return err
		}
	}

	if cfg.mcpToken == "" {
		log.Warn("MCP_TOKEN not set; /mcp is unauthenticated")
	}
	if cfg.searxngURL != "" {
		log.Info("web_search enabled", zap.String("searxng", cfg.searxngURL))
	}
	log.Info("listening",
		zap.String("addr", ln.Addr().String()),
		zap.String("upstream", cfg.baseURL),
		zap.String("version", version))

	// Child of a graceful restart: report readiness so the parent can exit.
	if readyFile != nil {
		if _, err := readyFile.Write([]byte("READY")); err != nil {
			return fmt.Errorf("signaling readiness to parent: %w", err)
		}
	}

	serve := func() error {
		err := httpSrv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	drain := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Warn("shutdown drained with error", zap.Error(err))
		}
	}

	err = supervisor.Serve(ln, serve, drain)
	if errors.Is(err, supervisor.ErrRestart) {
		log.Info("restart handoff complete; exiting")
		return nil
	}
	return err
}

// hardExit is intentionally minimal: plain SIGINT/SIGTERM terminate the
// process; graceful restarts go through the supervisor (SIGUSR1).
var (
	_ = os.Interrupt
	_ = syscall.SIGTERM
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}
