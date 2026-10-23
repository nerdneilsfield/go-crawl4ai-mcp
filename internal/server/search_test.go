package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serveMCP wires a server over stateless streamable HTTP and returns a POST helper.
func serveMCP(t *testing.T, opts Options) func(body string) string {
	t.Helper()
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true,
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	post := func(body string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	post(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	return post
}

func TestNoSearXNGConfigKeepsOriginalTools(t *testing.T) {
	post := serveMCP(t, Options{Crawl4AIBaseURL: "http://127.0.0.1:1", EnableAsk: true})
	out := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	for _, want := range []string{"md", "html", "crawl", "screenshot", "pdf", "ask", "submit_crawl", "get_crawl_job"} {
		if !strings.Contains(out, `"`+want+`"`) {
			t.Fatalf("tool %s missing from tools/list", want)
		}
	}
	if strings.Contains(out, `"web_search"`) {
		t.Fatal("web_search registered without SEARXNG_URL")
	}
}

func TestWebSearchToolRegisteredAndCallable(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"query":"fpga","results":[{"title":"t1","url":"https://a/","content":"s1","engines":["ddg"],"score":1}],"unresponsive_engines":[{"engine":"google","error":"CAPTCHA"}]}`))
	}))
	t.Cleanup(stub.Close)

	post := serveMCP(t, Options{Crawl4AIBaseURL: "http://127.0.0.1:1", SearXNGBaseURL: stub.URL})
	list := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if !strings.Contains(list, `"web_search"`) {
		t.Fatalf("web_search missing from tools/list: %.300s", list)
	}

	res := post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"fpga"}}}`)
	for _, want := range []string{`"structuredContent"`, `"url":"https://a/"`, `"google"`, `"CAPTCHA"`} {
		if !strings.Contains(res, want) {
			t.Fatalf("web_search result missing %s: %.400s", want, res)
		}
	}
}

func TestWebSearchMaxResultsTruncates(t *testing.T) {
	// 30 hits from the engine; default cap 10 → exactly 10 in tool output.
	var payload strings.Builder
	payload.WriteString(`{"query":"q","results":[`)
	for i := 0; i < 30; i++ {
		if i > 0 {
			payload.WriteString(",")
		}
		payload.WriteString(`{"title":"t","url":"https://x/` + string(rune('a'+i%26)) + string(rune('0'+i%10)) + `","content":"c","engines":["e"],"score":1}`)
	}
	payload.WriteString(`]}`)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload.String()))
	}))
	t.Cleanup(stub.Close)

	post := serveMCP(t, Options{Crawl4AIBaseURL: "http://127.0.0.1:1", SearXNGBaseURL: stub.URL})
	res := post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q"}}}`)
	if got := strings.Count(res, `"title"`); got != 10 {
		t.Fatalf("title count = %d, want 10 (default cap)", got)
	}
	// explicit over-cap request clamps to 20
	res = post(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q","max_results":50}}}`)
	if got := strings.Count(res, `"title"`); got != 20 {
		t.Fatalf("title count = %d, want 20 (hard cap)", got)
	}
}

func TestWebSearchUpstreamErrorSurfaces(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(stub.Close)
	post := serveMCP(t, Options{Crawl4AIBaseURL: "http://127.0.0.1:1", SearXNGBaseURL: stub.URL})
	res := post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q"}}}`)
	if !strings.Contains(res, `"isError":true`) || !strings.Contains(res, "json format not enabled") {
		t.Fatalf("expected tool error with hint: %.400s", res)
	}
}

// providerStubs returns a DeepSeek and a SearXNG stub that each mark their
// hits with a distinct title, plus the last DeepSeek request body.
func providerStubs(t *testing.T) (ds, sx *httptest.Server, dsBody *string) {
	t.Helper()
	body := new(string)
	ds = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*body = string(b)
		_, _ = w.Write([]byte(`{"results":[{"rerank_score":2.5,"title":"from-deepseek","url":"https://d/","snippet":"s","summary":"long","date":"2026-01-02T00:00:00Z","authority":3}]}`))
	}))
	sx = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"query":"q","results":[{"title":"from-searxng","url":"https://s/","content":"c"}]}`))
	}))
	t.Cleanup(ds.Close)
	t.Cleanup(sx.Close)
	return ds, sx, body
}

func TestWebSearchProviderSelection(t *testing.T) {
	ds, sx, _ := providerStubs(t)
	call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q"}}}`
	for _, tc := range []struct {
		name     string
		opts     Options
		want     string
		wantNone string
	}{
		{"auto with key uses deepseek", Options{DeepSeekAPIKey: "k", SearXNGBaseURL: sx.URL}, "from-deepseek", "from-searxng"},
		{"auto without key uses searxng", Options{SearXNGBaseURL: sx.URL}, "from-searxng", "from-deepseek"},
		{"explicit searxng beats key", Options{SearchProvider: "searxng", DeepSeekAPIKey: "k", SearXNGBaseURL: sx.URL}, "from-searxng", "from-deepseek"},
		{"explicit deepseek", Options{SearchProvider: "deepseek", DeepSeekAPIKey: "k", SearXNGBaseURL: sx.URL}, "from-deepseek", "from-searxng"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Crawl4AIBaseURL = "http://[IP]:1"
			tc.opts.DeepSeekBaseURL = ds.URL
			res := serveMCP(t, tc.opts)(call)
			if !strings.Contains(res, tc.want) || strings.Contains(res, tc.wantNone) {
				t.Fatalf("want %s only: %.400s", tc.want, res)
			}
		})
	}
}

func TestWebSearchProviderMisconfigured(t *testing.T) {
	for _, opts := range []Options{
		{SearchProvider: "deepseek"},
		{SearchProvider: "searxng", DeepSeekAPIKey: "k"},
		{SearchProvider: "bing", DeepSeekAPIKey: "k"},
	} {
		opts.Crawl4AIBaseURL = "http://[IP]:1"
		if _, err := New(opts); err == nil {
			t.Fatalf("New(%+v) should fail", opts)
		}
	}
}

func TestWebSearchDeepSeekMapping(t *testing.T) {
	ds, _, body := providerStubs(t)
	post := serveMCP(t, Options{Crawl4AIBaseURL: "http://[IP]:1", DeepSeekAPIKey: "k", DeepSeekBaseURL: ds.URL})
	res := post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q","language":"zh-CN","time_range":"week","max_results":7,"page":2}}}`)
	for _, want := range []string{`"summary":"long"`, `"rerankScore":2.5`, `"authority":3`, `"publishedDate":"2026-01-02T00:00:00Z"`, `"notice"`} {
		if !strings.Contains(res, want) {
			t.Fatalf("result missing %s: %.600s", want, res)
		}
	}
	after := time.Now().AddDate(0, 0, -7).Format("2006-01-02")
	for _, want := range []string{`q lang:zh after:` + after, `"count":7`} {
		if !strings.Contains(*body, want) {
			t.Fatalf("request body missing %s: %s", want, *body)
		}
	}
}
