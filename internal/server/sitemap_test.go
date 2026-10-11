package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func callSitemap(t *testing.T, in sitemapIn) (sitemapOut, error) {
	t.Helper()
	handler := sitemapURLsTool()
	var raw []byte
	if b, err := json.Marshal(in); err == nil {
		raw = b
	} else {
		t.Fatal(err)
	}
	var typed sitemapIn
	if err := json.Unmarshal(raw, &typed); err != nil {
		t.Fatal(err)
	}
	res, out, err := handler(context.Background(), &mcp.CallToolRequest{}, typed)
	if res != nil && res.IsError {
		return sitemapOut{}, err
	}
	return out, err
}

const urlsetXML = `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/a</loc></url>
  <url><loc>https://example.com/b</loc></url>
  <!-- comment between entries -->
  <url><loc>https://example.com/c</loc><lastmod>2026-01-01</lastmod></url>
</urlset>`

const indexXML = `<?xml version="1.0" encoding="UTF-8"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>https://example.com/sitemap-products.xml</loc></sitemap>
  <sitemap><loc>https://example.com/sitemap-blog.xml</loc></sitemap>
</sitemapindex>`

func TestSitemapURLOverview(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sitemap.xml":
			_, _ = w.Write([]byte(urlsetXML))
		case "/robots.txt":
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /private\n\nSitemap: http://" + r.Host + "/sitemap.xml\n"))
		case "/index.xml":
			_, _ = w.Write([]byte(indexXML))
		case "/list.txt":
			_, _ = w.Write([]byte("# comment\nhttps://example.com/x\n\nhttps://example.com/y\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// urlset
	out, err := callSitemap(t, sitemapIn{URL: srv.URL + "/sitemap.xml"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Type != "urlset" || out.Count != 3 || out.Truncated {
		t.Fatalf("urlset: %+v", out)
	}
	if out.URLs[0] != "https://example.com/a" || out.URLs[2] != "https://example.com/c" {
		t.Fatalf("urls: %v", out.URLs)
	}

	// robots.txt -> follows first Sitemap: line
	out, err = callSitemap(t, sitemapIn{URL: srv.URL + "/robots.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Type != "urlset" || out.Count != 3 {
		t.Fatalf("robots: %+v", out)
	}

	// sitemapindex: returns child sitemap locs
	out, err = callSitemap(t, sitemapIn{URL: srv.URL + "/index.xml"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Type != "sitemapindex" || out.Count != 2 {
		t.Fatalf("index: %+v", out)
	}
	if out.URLs[0] != "https://example.com/sitemap-products.xml" {
		t.Fatalf("index urls: %v", out.URLs)
	}

	// plain text list
	out, err = callSitemap(t, sitemapIn{URL: srv.URL + "/list.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Type != "text" || out.Count != 2 || out.URLs[1] != "https://example.com/y" {
		t.Fatalf("text: %+v", out)
	}
}

func TestSitemapTruncationAndErrors(t *testing.T) {
	var body string
	var status int = http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	// truncation
	body = "<?xml version=\"1.0\"?><urlset>"
	for i := 0; i < 50; i++ {
		body += "<url><loc>https://example.com/" + itoa(i) + "</loc></url>"
	}
	body += "</urlset>"
	out, err := callSitemap(t, sitemapIn{URL: srv.URL + "/s.xml", MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 10 || !out.Truncated {
		t.Fatalf("truncation: %+v", out)
	}

	// 404
	body = ""
	status = http.StatusNotFound
	if _, err := callSitemap(t, sitemapIn{URL: srv.URL + "/missing.xml"}); err == nil {
		t.Fatal("expected error for 404")
	}
	status = http.StatusOK

	// robots.txt without Sitemap lines
	body = "User-agent: *\nDisallow: /"
	if _, err := callSitemap(t, sitemapIn{URL: srv.URL + "/robots.txt"}); err == nil {
		t.Fatal("expected error for robots.txt without sitemaps")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
