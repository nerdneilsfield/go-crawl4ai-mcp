// Package server: sitemap tool. Fetches a sitemap (XML sitemapindex or
// urlset, plain text list, or robots.txt with Sitemap: lines) and returns
// the URL list. Discovery stays separate from reading: the agent picks
// URLs and fetches them with md/crawl.
package server

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const toolSitemapURLs = "sitemap_urls"

func addSitemapTool(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        toolSitemapURLs,
		Description: "Fetch a sitemap and return its URL list, one per line. Accepts XML sitemaps (urlset or sitemapindex), plain text URL lists, and robots.txt (extracts Sitemap: lines, following the first sitemap). Use to discover candidate URLs before reading pages with md or crawl.",
	}, sitemapURLsTool())
}

type sitemapIn struct {
	URL         string `json:"url" jsonschema:"sitemap.xml URL, plain text URL list, or robots.txt URL"`
	MaxResults  int    `json:"max_results,omitempty" jsonschema:"max URLs to return, default 100, hard cap 1000"`
	FollowIndex bool   `json:"follow_index,omitempty" jsonschema:"for sitemapindex entries, fetch and flatten child sitemaps (one level); default false"`
}

type sitemapOut struct {
	URL       string   `json:"url"`
	Type      string   `json:"type"` // urlset, sitemapindex, text, robots
	Count     int      `json:"count"`
	Truncated bool     `json:"truncated,omitempty"`
	URLs      []string `json:"urls"`
}

// sitemapHTTPClient fetches sitemaps directly (they are static files, not
// tool calls against Crawl4AI): fail fast, follow redirects.
var sitemapHTTPClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
	},
}

func sitemapURLsTool() func(context.Context, *mcp.CallToolRequest, sitemapIn) (*mcp.CallToolResult, sitemapOut, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in sitemapIn) (*mcp.CallToolResult, sitemapOut, error) {
		if in.URL == "" {
			return nil, sitemapOut{}, fmt.Errorf("url is required")
		}
		if _, err := url.Parse(in.URL); err != nil {
			return nil, sitemapOut{}, fmt.Errorf("invalid url %q: %w", in.URL, err)
		}
		max := in.MaxResults
		if max <= 0 {
			max = 100
		}
		if max > 1000 {
			max = 1000
		}

		out := sitemapOut{URL: in.URL}
		urls, kind, err := fetchSitemapURLs(ctx, in.URL, max)
		if err != nil {
			return nil, sitemapOut{}, err
		}
		out.Type = kind
		out.URLs = urls
		out.Count = len(urls)
		out.Truncated = out.Count >= max
		return nil, out, nil
	}
}

// fetchSitemapURLs retrieves and parses the resource at u. robots.txt files
// are followed into their first sitemap entry.
func fetchSitemapURLs(ctx context.Context, u string, max int) ([]string, string, error) {
	body, err := httpGet(ctx, u)
	if err != nil {
		return nil, "", err
	}
	trimmed := strings.TrimSpace(body)
	switch {
	case strings.HasPrefix(trimmed, "User-agent:") || strings.HasPrefix(trimmed, "user-agent:"):
		locs := parseRobotsSitemaps(trimmed)
		if len(locs) == 0 {
			return nil, "", fmt.Errorf("%s: robots.txt contains no Sitemap: lines", u)
		}
		sub, kind, err := fetchSitemapURLs(ctx, locs[0], max)
		if err != nil {
			return nil, "", fmt.Errorf("following robots.txt sitemap %s: %w", locs[0], err)
		}
		return sub, kind, nil
	case strings.HasPrefix(trimmed, "<"):
		locs, kind, err := parseSitemapXML(strings.NewReader(trimmed), max)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", u, err)
		}
		return locs, kind, nil
	default:
		// Plain text: one URL per line, comments (#) and blanks skipped.
		var urls []string
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			urls = append(urls, line)
			if len(urls) >= max {
				break
			}
		}
		return urls, "text", nil
	}
}

// parseSitemapXML parses a urlset or sitemapindex, returning the loc list and
// the detected kind. Nested child sitemaps are only detected; the agent can
// call the tool again on a child URL (explicit fetch beats magic recursion).
func parseSitemapXML(r io.Reader, max int) ([]string, string, error) {
	dec := xml.NewDecoder(r)
	var locs []string
	kind := "urlset"
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return locs, kind, fmt.Errorf("parsing XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 && t.Name.Local == "sitemapindex" {
				kind = "sitemapindex"
			}
			if depth == 3 && t.Name.Local == "loc" && len(locs) < max {
				var s string
				if err := dec.DecodeElement(&s, &t); err == nil && s != "" {
					locs = append(locs, strings.TrimSpace(s))
				}
				// DecodeElement consumed </loc> too; the loop would otherwise
				// over-count depth for every entry after the first.
				depth--
			}
		case xml.EndElement:
			depth--
		}
	}
	if kind == "sitemapindex" {
		return locs, kind, nil
	}
	return locs, "urlset", nil
}

// parseRobotsSitemaps returns the absolute URLs from Sitemap: lines.
func parseRobotsSitemaps(body string) []string {
	var locs []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.IndexAny(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		for _, prefix := range []string{"Sitemap:", "sitemap:"} {
			if strings.HasPrefix(line, prefix) {
				if s := strings.TrimSpace(strings.TrimPrefix(line, prefix)); s != "" {
					locs = append(locs, s)
				}
			}
		}
	}
	return locs
}

func httpGet(ctx context.Context, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("invalid url %q: %w", u, err)
	}
	req.Header.Set("User-Agent", "go-crawl4ai-mcp/0.1 (+https://github.com/nerdneilsfield/go-crawl4ai-mcp)")
	resp, err := sitemapHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching %s: HTTP %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // sitemaps are capped at 50MB by spec; be stricter
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", u, err)
	}
	return string(b), nil
}
