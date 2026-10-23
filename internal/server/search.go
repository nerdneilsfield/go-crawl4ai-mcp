package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/deepseek"
	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/searxng"
)

const toolWebSearch = "web_search"

const (
	providerAuto     = "auto"
	providerDeepSeek = "deepseek"
	providerSearXNG  = "searxng"
)

// addSearchTool registers web_search when a backend is available, choosing it
// per opts.SearchProvider. Searching and reading stay separate: the tool only
// returns links and snippets; the agent picks which URLs to pass to md/crawl.
func addSearchTool(s *mcp.Server, opts Options) error {
	provider := opts.SearchProvider
	switch provider {
	case "", providerAuto:
		switch {
		case opts.DeepSeekAPIKey != "":
			provider = providerDeepSeek
		case opts.SearXNGBaseURL != "":
			provider = providerSearXNG
		default:
			return nil // no backend configured: no web_search tool
		}
	case providerDeepSeek:
		if opts.DeepSeekAPIKey == "" {
			return fmt.Errorf("search provider deepseek requires DEEPSEEK_API_KEY")
		}
	case providerSearXNG:
		if opts.SearXNGBaseURL == "" {
			return fmt.Errorf("search provider searxng requires SEARXNG_URL")
		}
	default:
		return fmt.Errorf("invalid search provider %q (auto/deepseek/searxng)", provider)
	}

	t := &mcp.Tool{Name: toolWebSearch}
	var search func(context.Context, webSearchIn, int) (webSearchOut, error)
	if provider == providerDeepSeek {
		cl := &deepseek.Client{BaseURL: opts.DeepSeekBaseURL, APIKey: opts.DeepSeekAPIKey}
		t.Description = "Web search via the DeepSeek Search API (own index + rerank). Returns titles, URLs, snippets, a longer summary and a rerank_score (>=1.5 is high quality) — follow up with the md or crawl tool to read chosen pages. The query accepts operators: site:, -site:, \"phrase\", -word, lang:zh,en, after:2025, before:2026. language and time_range are mapped to lang:/after: operators; categories and page are not supported."
		search = func(ctx context.Context, in webSearchIn, max int) (webSearchOut, error) {
			return deepseekSearch(ctx, cl, in, max, time.Now())
		}
	} else {
		cl := &searxng.Client{BaseURL: opts.SearXNGBaseURL}
		t.Description = "Web search via SearXNG meta-search. Returns titles, URLs and snippets only — follow up with the md or crawl tool to read chosen pages. Supports filtering by language, category (news, science, ...) and time range."
		search = func(ctx context.Context, in webSearchIn, max int) (webSearchOut, error) {
			return searxngSearch(ctx, cl, in, max)
		}
	}
	mcp.AddTool(s, t, func(ctx context.Context, _ *mcp.CallToolRequest, in webSearchIn) (*mcp.CallToolResult, webSearchOut, error) {
		max := in.MaxResults
		if max <= 0 {
			max = 10
		}
		if max > 20 {
			max = 20
		}
		out, err := search(ctx, in, max)
		if err != nil {
			return nil, webSearchOut{}, err
		}
		return nil, out, nil
	})
	return nil
}

type webSearchIn struct {
	Query      string `json:"query" jsonschema:"search keywords (required)"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"max results to return, default 10, hard cap 20 (SearXNG: truncates the current page)"`
	Language   string `json:"language,omitempty" jsonschema:"result language, e.g. zh-CN, en; backend default when empty (DeepSeek: becomes a lang: operator)"`
	Categories string `json:"categories,omitempty" jsonschema:"search categories, e.g. general, news, science; default general (SearXNG only, ignored by DeepSeek)"`
	TimeRange  string `json:"time_range,omitempty" jsonschema:"restrict to day, week, month or year; unlimited when empty (DeepSeek: becomes an after: operator)"`
	Page       int    `json:"page,omitempty" jsonschema:"result page number, 1-based; default 1 (SearXNG only)"`
}

type webSearchResult struct {
	Title       string   `json:"title"`
	URL         string   `json:"url"`
	Snippet     string   `json:"snippet,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Engines     []string `json:"engines,omitempty"`
	PublishedAt string   `json:"publishedDate,omitempty"`
	RerankScore *float64 `json:"rerankScore,omitempty"`
	Authority   *int     `json:"authority,omitempty"`
}

type webSearchOut struct {
	Query               string                       `json:"query"`
	Results             []webSearchResult            `json:"results"`
	UnresponsiveEngines []searxng.UnresponsiveEngine `json:"unresponsive_engines,omitempty"`
	Notice              string                       `json:"notice,omitempty"`
}

func searxngSearch(ctx context.Context, cl *searxng.Client, in webSearchIn, max int) (webSearchOut, error) {
	resp, err := cl.Search(ctx, searxng.SearchRequest{
		Query:      in.Query,
		Language:   in.Language,
		Categories: in.Categories,
		TimeRange:  in.TimeRange,
		Page:       in.Page,
	})
	if err != nil {
		return webSearchOut{}, err
	}
	out := webSearchOut{Query: resp.Query, UnresponsiveEngines: resp.UnresponsiveEngines}
	n := max
	if len(resp.Results) < n {
		n = len(resp.Results)
	}
	out.Results = make([]webSearchResult, 0, n)
	for _, r := range resp.Results[:n] {
		out.Results = append(out.Results, webSearchResult{
			Title: r.Title, URL: r.URL, Snippet: r.Snippet,
			Engines: r.Engines, PublishedAt: r.PublishedAt,
		})
	}
	return out, nil
}

// deepseekSearch maps the tool arguments onto DeepSeek's query operators:
// DeepSeek has no language/time_range/categories/page parameters.
func deepseekSearch(ctx context.Context, cl *deepseek.Client, in webSearchIn, max int, now time.Time) (webSearchOut, error) {
	q := in.Query
	if lang := strings.ToLower(strings.TrimSpace(in.Language)); lang != "" {
		if i := strings.IndexAny(lang, "-_"); i >= 0 {
			lang = lang[:i]
		}
		q += " lang:" + lang
	}
	var after time.Time
	switch in.TimeRange {
	case "":
	case "day":
		after = now.AddDate(0, 0, -1)
	case "week":
		after = now.AddDate(0, 0, -7)
	case "month":
		after = now.AddDate(0, -1, 0)
	case "year":
		after = now.AddDate(-1, 0, 0)
	default:
		return webSearchOut{}, fmt.Errorf("invalid time_range %q (day/week/month/year)", in.TimeRange)
	}
	if !after.IsZero() {
		q += " after:" + after.Format("2006-01-02")
	}

	resp, err := cl.Search(ctx, deepseek.SearchRequest{Query: q, Count: max})
	if err != nil {
		return webSearchOut{}, err
	}
	out := webSearchOut{Query: in.Query, Results: make([]webSearchResult, 0, len(resp.Results))}
	if in.Page > 1 {
		out.Notice = "the DeepSeek backend does not support pagination; page was ignored"
	}
	for _, r := range resp.Results {
		score, authority := r.RerankScore, r.Authority
		out.Results = append(out.Results, webSearchResult{
			Title: r.Title, URL: r.URL, Snippet: r.Snippet, Summary: r.Summary,
			PublishedAt: r.Date, RerankScore: &score, Authority: &authority,
		})
	}
	if len(out.Results) > max {
		out.Results = out.Results[:max]
	}
	return out, nil
}
