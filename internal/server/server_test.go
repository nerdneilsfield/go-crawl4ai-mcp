package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/c4a"
)

// summarizeCrawl must preserve per-page failures even when the upstream
// top-level success is true (a real /crawl behavior observed in server.py).
func TestSummarizeCrawlPartialFailure(t *testing.T) {
	resp := &c4a.CrawlResponse{
		Success: true,
		Results: []c4a.CrawlResult{
			{URL: "https://a/", Success: true, Markdown: json.RawMessage(`"md a"`)},
			{URL: "https://b/", Success: false, ErrorMessage: "boom"},
		},
	}
	out := summarizeCrawl(resp, false)
	if out.Success {
		t.Fatal("top-level success must be false when a page fails")
	}
	if out.FailedCount != 1 {
		t.Fatalf("failed_count = %d, want 1", out.FailedCount)
	}
	if out.Pages[1].Error != "boom" {
		t.Fatalf("per-page error lost: %+v", out.Pages[1])
	}
	if out.Pages[0].Markdown != "md a" {
		t.Fatalf("markdown lost: %+v", out.Pages[0])
	}
}

// The upstream /md endpoint returns markdown as a plain string; the crawl
// endpoints return an object. MarkdownString must handle both.
func TestMarkdownUnion(t *testing.T) {
	if got := c4a.MarkdownString(json.RawMessage(`"x"`)); got != "x" {
		t.Fatalf("string form: got %q", got)
	}
	if got := c4a.MarkdownString(json.RawMessage(`{"raw_markdown":"y"}`)); got != "y" {
		t.Fatalf("object form: got %q", got)
	}
	if got := c4a.MarkdownString(nil); got != "" {
		t.Fatalf("nil form: got %q", got)
	}
}

func TestAskBuildsQueryAndParses(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"doc_context":"docs"}`))
	}))
	defer srv.Close()
	cl := &c4a.Client{BaseURL: srv.URL}
	resp, err := cl.Ask(context.Background(), "cache mode", "", 0.5, 20)
	if err != nil {
		t.Fatal(err)
	}
	if resp.DocContext != "docs" {
		t.Fatalf("doc context: %q", resp.DocContext)
	}
	for _, want := range []string{"context_type=all", "query=cache", "score_ratio=0.5", "max_results=20"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("query %q missing %q", gotQuery, want)
		}
	}
}
