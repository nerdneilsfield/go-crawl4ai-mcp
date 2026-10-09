// Package searxng is a minimal client for a SearXNG instance's JSON search API
// (GET /search?format=json). The instance must enable the json format in its
// settings.yml, otherwise the endpoint answers 403.
package searxng

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/appconfig"
)

// Client talks to a SearXNG instance.
type Client struct {
	BaseURL string // e.g. "http://searxng:8080"
	HC      *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HC != nil {
		return c.HC
	}
	// Shared client: search must fail fast, independent of crawl timeouts.
	return appconfig.SearchHTTPClient
}

// SearchRequest carries the /search query parameters this client supports.
type SearchRequest struct {
	Query      string // required
	Language   string // e.g. "zh-CN", "en"; instance default when empty
	Categories string // e.g. "general", "news", "science"; default general
	TimeRange  string // "", day, week, month, year
	Page       int    // 1-based; 0 means 1
}

// Result is one aggregated hit.
type Result struct {
	Title       string   `json:"title"`
	URL         string   `json:"url"`
	Snippet     string   `json:"snippet"`
	Engines     []string `json:"engines,omitempty"`
	Score       float64  `json:"score,omitempty"`
	PublishedAt string   `json:"publishedDate,omitempty"`
}

// UnresponsiveEngine describes a backend engine that failed for this search.
type UnresponsiveEngine struct {
	Engine string `json:"engine"`
	Error  string `json:"error,omitempty"`
}

// Response is the subset of SearXNG's JSON payload we surface.
type Response struct {
	Query               string               `json:"query"`
	Results             []Result             `json:"results"`
	UnresponsiveEngines []UnresponsiveEngine `json:"unresponsive_engines,omitempty"`
}

// Error is a non-2xx upstream response.
type Error struct {
	StatusCode int
	Detail     string
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("searxng: HTTP %d: %s", e.StatusCode, e.Detail)
	}
	return fmt.Sprintf("searxng: HTTP %d", e.StatusCode)
}

// Search runs a query and returns aggregated results. Engine-level failures
// (unresponsive_engines) are returned alongside results, not as an error.
func (c *Client) Search(ctx context.Context, req SearchRequest) (*Response, error) {
	if strings.TrimSpace(req.Query) == "" {
		return nil, fmt.Errorf("searxng: empty query")
	}
	q := url.Values{}
	q.Set("q", req.Query)
	q.Set("format", "json")
	if req.Language != "" {
		q.Set("language", req.Language)
	}
	if req.Categories != "" {
		q.Set("categories", req.Categories)
	}
	switch req.TimeRange {
	case "", "day", "week", "month", "year":
		if req.TimeRange != "" {
			q.Set("time_range", req.TimeRange)
		}
	default:
		return nil, fmt.Errorf("searxng: invalid time_range %q (day/week/month/year)", req.TimeRange)
	}
	page := req.Page
	if page < 1 {
		page = 1
	}
	q.Set("pageno", fmt.Sprint(page))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, &Error{StatusCode: resp.StatusCode, Detail: "json format not enabled on the SearXNG instance (add 'json' to search.formats in settings.yml)"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := strings.TrimSpace(string(data))
		if len(detail) > 300 {
			detail = detail[:300]
		}
		return nil, &Error{StatusCode: resp.StatusCode, Detail: detail}
	}

	var raw struct {
		Query   string `json:"query"`
		Results []struct {
			Title       string   `json:"title"`
			URL         string   `json:"url"`
			Content     string   `json:"content"`
			Engines     []string `json:"engines"`
			Score       float64  `json:"score"`
			PublishedAt string   `json:"publishedDate"`
		} `json:"results"`
		UnresponsiveEngines []json.RawMessage `json:"unresponsive_engines"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("searxng: decoding response: %w", err)
	}

	out := &Response{Query: raw.Query, Results: make([]Result, 0, len(raw.Results))}
	for _, r := range raw.Results {
		out.Results = append(out.Results, Result{
			Title: r.Title, URL: r.URL, Snippet: r.Content,
			Engines: r.Engines, Score: r.Score, PublishedAt: r.PublishedAt,
		})
	}
	for _, u := range raw.UnresponsiveEngines {
		if eng, msg, ok := parseUnresponsive(u); ok {
			out.UnresponsiveEngines = append(out.UnresponsiveEngines, UnresponsiveEngine{Engine: eng, Error: msg})
		}
	}
	return out, nil
}

// parseUnresponsive accepts the upstream pair form ("[\"google\",\"CAPTCHA\"]",
// from get_translated_errors in searx/webutils.py) and an object form
// ({"engine":..., "error":...}) for forward compatibility.
func parseUnresponsive(raw json.RawMessage) (engine, msg string, ok bool) {
	var pair []string
	if json.Unmarshal(raw, &pair) == nil && len(pair) >= 1 {
		if len(pair) >= 2 {
			return pair[0], pair[1], true
		}
		return pair[0], "", true
	}
	var obj struct {
		Engine string `json:"engine"`
		Error  string `json:"error"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Engine != "" {
		return obj.Engine, obj.Error, true
	}
	return "", "", false
}
