package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/c4a"
)

// backend answers /crawl with one successful page per requested URL, tagged
// with its own name, and /crawl/job with a fixed task id.
func backend(t *testing.T, name string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/crawl":
			body, _ := io.ReadAll(r.Body)
			var urls []string
			for _, u := range []string{"a.cn", "b.cn", "c.com", "10.1.2.3"} {
				if strings.Contains(string(body), u) {
					urls = append(urls, u)
				}
			}
			w.Write([]byte(`{"success":true,"results":[`))
			for i, u := range urls {
				if i > 0 {
					w.Write([]byte(","))
				}
				w.Write([]byte(`{"url":"` + u + `","success":true,"markdown":"` + name + `"}`))
			}
			w.Write([]byte(`]}`))
		case r.URL.Path == "/crawl/job":
			w.Write([]byte(`{"task_id":"t1","status":"processing"}`))
		case strings.HasPrefix(r.URL.Path, "/crawl/job/"):
			w.Write([]byte(`{"task_id":"` + strings.TrimPrefix(r.URL.Path, "/crawl/job/") + `","status":"` + name + `"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRouter(t *testing.T) {
	hk, us := backend(t, "hk"), backend(t, "us")
	cfg := filepath.Join(t.TempDir(), "r.yaml")
	os.WriteFile(cfg, []byte(`
backends:
  hk: {url: `+hk.URL+`}
  us: {url: `+us.URL+`}
rules:
  - DOMAIN-SUFFIX,cn,hk
  - IP-CIDR,10.0.0.0/8,hk
  - MATCH,us
`), 0o600)
	rt, err := loadRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}

	for url, want := range map[string]string{
		"https://a.cn/x": "hk", "https://www.a.cn": "hk", "https://nota.cn.com": "us",
		"http://10.1.2.3:8080/": "hk", "http://11.1.2.3/": "us", "https://c.com": "us", "raw:<html>": "us", "raw://a.cn/x": "us",
	} {
		if got := rt.pick(context.Background(), url); got != want {
			t.Errorf("pick(%q) = %s, want %s", url, got, want)
		}
	}

	resp, err := rt.crawl(context.Background(), c4a.CrawlRequest{URLs: []string{"https://a.cn", "https://c.com", "https://b.cn"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range resp.Results {
		got[p.URL] = string(p.Markdown)
	}
	if len(got) != 3 || got["a.cn"] != `"hk"` || got["b.cn"] != `"hk"` || got["c.com"] != `"us"` {
		t.Errorf("crawl split wrong: %v", got)
	}

	if _, err := rt.submit(context.Background(), c4a.CrawlJobRequest{URLs: []string{"https://a.cn", "https://c.com"}}); err == nil {
		t.Error("submit across backends must fail")
	}
	id, err := rt.submit(context.Background(), c4a.CrawlJobRequest{URLs: []string{"https://c.com"}})
	if err != nil || id != "us:t1" {
		t.Fatalf("submit id = %q, %v", id, err)
	}
	st, err := rt.job(context.Background(), id)
	if err != nil || st.Status != "us" || st.TaskID != id {
		t.Errorf("job round-trip: %+v, %v", st, err)
	}
}

func TestLoadRouterRejects(t *testing.T) {
	for name, y := range map[string]string{
		"unknown backend": "backends: {a: {url: http://x}}\nrules: [\"MATCH,b\"]",
		"no match":        "backends: {a: {url: http://x}}\nrules: [\"DOMAIN,x.com,a\"]",
		"match not last":  "backends: {a: {url: http://x}}\nrules: [\"MATCH,a\", \"DOMAIN,x.com,a\"]",
		"bad cidr":        "backends: {a: {url: http://x}}\nrules: [\"IP-CIDR,nope,a\", \"MATCH,a\"]",
		"colon in name":   "backends: {\"a:b\": {url: http://x}}\nrules: [\"MATCH,a:b\"]",
	} {
		p := filepath.Join(t.TempDir(), "r.yaml")
		os.WriteFile(p, []byte(y), 0o600)
		if _, err := loadRouter(p); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
