package c4a

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/appconfig"
)

// The per-call deadline must apply and produce a timeout error, while 0
// disables it. Caller-context cancellation must still work alongside.
func TestCrawlTimeoutAppliesAndDisables(t *testing.T) {
	// Handler holds the response open; the goroutine leak is bounded by the
	// test server's own Close (it waits for handlers), so keep waits short.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)

	orig := appconfig.CrawlTimeout
	defer func() { appconfig.CrawlTimeout = orig }()

	appconfig.CrawlTimeout = 200 * time.Millisecond
	cl := &Client{BaseURL: slow.URL, HC: &http.Client{}}
	start := time.Now()
	_, err := cl.Crawl(context.Background(), CrawlRequest{URLs: []string{"https://x/"}})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("timeout did not apply: %v", time.Since(start))
	}

	appconfig.CrawlTimeout = 0
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, err = cl.Crawl(ctx, CrawlRequest{URLs: []string{"https://x/"}})
	if err == nil {
		t.Fatal("expected caller ctx cancellation")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("caller ctx did not apply: %v", time.Since(start))
	}
}
