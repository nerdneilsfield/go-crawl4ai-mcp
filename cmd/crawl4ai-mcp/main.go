package main

import (
	"crypto/subtle"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/server"
)

var version = "0.1.0"

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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

func main() {
	addr := flag.String("addr", envOr("MCP_ADDR", ":8080"), "listen address")
	baseURL := flag.String("crawl4ai-url", envOr("CRAWL4AI_URL", "http://127.0.0.1:11235"), "Crawl4AI server base URL")
	c4aToken := flag.String("crawl4ai-token", os.Getenv("CRAWL4AI_TOKEN"), "Crawl4AI API token (CRAWL4AI_API_TOKEN equivalent)")
	mcpToken := flag.String("mcp-token", os.Getenv("MCP_TOKEN"), "bearer token required by MCP clients; empty disables auth")
	searxngURL := flag.String("searxng-url", os.Getenv("SEARXNG_URL"), "SearXNG base URL; registers the web_search tool when set (instance must enable the json format)")
	flag.Parse()

	srv, err := server.New(server.Options{
		Crawl4AIBaseURL: *baseURL,
		Crawl4AIToken:   *c4aToken,
		EnableExecuteJS: envOr("ENABLE_EXECUTE_JS", "") == "true",
		EnableAsk:       envOr("ENABLE_ASK", "true") == "true",
		SearXNGBaseURL:  strings.TrimRight(*searxngURL, "/"),
	})
	if err != nil {
		log.Fatalf("building server: %v", err)
	}

	// Stateless streamable HTTP: each POST is self-contained, GET/DELETE
	// return 405. JSONResponse avoids a standing SSE stream per request,
	// which keeps proxies and MCP clients (Bifrost, OpenCode) simple.
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return srv
	}, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		PropagateRequestCancellation: true, // client abort/timeout cancels in-flight upstream calls
	})

	mux := http.NewServeMux()
	mux.Handle("/mcp", bearerAuth(*mcpToken, handler))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"status":"ok","version":%q}`, version)
	})

	h := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if *mcpToken == "" {
		log.Printf("warning: MCP_TOKEN not set; /mcp is unauthenticated")
	}
	if *searxngURL != "" {
		log.Printf("web_search enabled via SearXNG at %s", *searxngURL)
	}
	log.Printf("crawl4ai-mcp %s listening on %s (upstream %s)", version, *addr, *baseURL)
	log.Fatal(h.ListenAndServe())
}
