package server

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/searxng"
)

const toolWebSearch = "web_search"

// addSearchTool registers the web_search tool when a SearXNG instance is
// configured. Searching and reading stay separate: the tool only returns
// links and snippets; the agent picks which URLs to pass to md/crawl.
func addSearchTool(s *mcp.Server, baseURL string) {
	cl := &searxng.Client{BaseURL: baseURL}
	mcp.AddTool(s, &mcp.Tool{
		Name:        toolWebSearch,
		Description: "Web search via SearXNG meta-search. Returns titles, URLs and snippets only — follow up with the md or crawl tool to read chosen pages. Supports filtering by language, category (news, science, ...) and time range.",
	}, webSearchTool(cl))
}

type webSearchIn struct {
	Query      string `json:"query" jsonschema:"search keywords (required)"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"max results to return, default 10, hard cap 20 (truncates the current SearXNG page)"`
	Language   string `json:"language,omitempty" jsonschema:"result language, e.g. zh-CN, en; instance default when empty"`
	Categories string `json:"categories,omitempty" jsonschema:"search categories, e.g. general, news, science; default general"`
	TimeRange  string `json:"time_range,omitempty" jsonschema:"restrict to day, week, month or year; unlimited when empty"`
	Page       int    `json:"page,omitempty" jsonschema:"result page number, 1-based; default 1"`
}

type webSearchResult struct {
	Title       string   `json:"title"`
	URL         string   `json:"url"`
	Snippet     string   `json:"snippet,omitempty"`
	Engines     []string `json:"engines,omitempty"`
	PublishedAt string   `json:"publishedDate,omitempty"`
}

type webSearchOut struct {
	Query               string                       `json:"query"`
	Results             []webSearchResult            `json:"results"`
	UnresponsiveEngines []searxng.UnresponsiveEngine `json:"unresponsive_engines,omitempty"`
}

func webSearchTool(cl *searxng.Client) func(context.Context, *mcp.CallToolRequest, webSearchIn) (*mcp.CallToolResult, webSearchOut, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in webSearchIn) (*mcp.CallToolResult, webSearchOut, error) {
		max := in.MaxResults
		if max <= 0 {
			max = 10
		}
		if max > 20 {
			max = 20
		}
		resp, err := cl.Search(ctx, searxng.SearchRequest{
			Query:      in.Query,
			Language:   in.Language,
			Categories: in.Categories,
			TimeRange:  in.TimeRange,
			Page:       in.Page,
		})
		if err != nil {
			return nil, webSearchOut{}, err
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
		return nil, out, nil
	}
}
