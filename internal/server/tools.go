// Package server wires Crawl4AI REST endpoints to MCP tools, mirroring the
// tool names and semantics of Crawl4AI's official mcp_bridge (md, html, crawl,
// screenshot, pdf, execute_js, ask) plus the async job endpoints.
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	_ "time/tzdata" // embed zoneinfo: distroless/static has no /usr/share/zoneinfo

	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/c4a"
)

// Options configures the MCP server.
type Options struct {
	Crawl4AIBaseURL string
	Crawl4AIToken   string
	RouteConfig     string // YAML file routing URLs to several Crawl4AI backends; overrides the two fields above
	EnableExecuteJS bool   // upstream requires CRAWL4AI_EXECUTE_JS_ENABLED=true on its side
	EnableAsk       bool
	SearXNGBaseURL  string // SearXNG backend for web_search
	DeepSeekAPIKey  string // DeepSeek Search backend for web_search
	DeepSeekBaseURL string // override for tests; empty = https://api.deepseek.com
	SearchProvider  string // auto (default: DeepSeek when a key is set, else SearXNG), deepseek, searxng
}

const (
	toolMD          = "md"
	toolHTML        = "html"
	toolCrawl       = "crawl"
	toolScreenshot  = "screenshot"
	toolPDF         = "pdf"
	toolExecuteJS   = "execute_js"
	toolAsk         = "ask"
	toolSubmitCrawl = "submit_crawl"
	toolGetCrawlJob = "get_crawl_job"
	toolCurrentTime = "current_time"
)

// New builds the MCP server with all tools registered.
func New(opts Options) (*mcp.Server, error) {
	rt := singleRouter(&c4a.Client{BaseURL: strings.TrimRight(opts.Crawl4AIBaseURL, "/"), Token: opts.Crawl4AIToken})
	if opts.RouteConfig != "" {
		var err error
		if rt, err = loadRouter(opts.RouteConfig); err != nil {
			return nil, err
		}
	}
	if rt.geo != nil {
		go rt.geo.run(context.Background()) // for the life of the process
	}

	s := mcp.NewServer(&mcp.Implementation{Name: "crawl4ai-mcp", Version: "0.2.1"}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        toolMD,
		Description: "Convert a web page into Markdown. Filters: fit (default, clean readable text), raw (full page), bm25/llm (relevance-filtered, requires q).",
	}, mdTool(rt))

	mcp.AddTool(s, &mcp.Tool{
		Name:        toolHTML,
		Description: "Crawl a URL and return sanitized HTML preprocessed for schema extraction.",
	}, htmlTool(rt))

	mcp.AddTool(s, &mcp.Tool{
		Name:        toolCrawl,
		Description: "Crawl one or more URLs (up to 100) and return Markdown plus metadata per page. Use browser_config/crawler_config for advanced Crawl4AI options; some per-page failures are reported individually.",
	}, crawlTool(rt))

	mcp.AddTool(s, &mcp.Tool{
		Name:        toolScreenshot,
		Description: "Capture a full-page PNG screenshot of a URL and return it as an image.",
	}, screenshotTool(rt))

	mcp.AddTool(s, &mcp.Tool{
		Name:        toolPDF,
		Description: "Render a URL to PDF and return it as an application/pdf resource blob.",
	}, pdfTool(rt))

	if opts.EnableExecuteJS {
		mcp.AddTool(s, &mcp.Tool{
			Name:        toolExecuteJS,
			Description: "Execute JavaScript snippets on a page and return the full CrawlResult (markdown, js_execution_result, links, ...). Each script must be an expression that returns a value (IIFE or async function).",
		}, executeJSTool(rt))
	}

	if opts.EnableAsk {
		mcp.AddTool(s, &mcp.Tool{
			Name:        toolAsk,
			Description: "Query the Crawl4AI project's own documentation and source context (BM25-filtered). Not a general web Q&A tool.",
		}, askTool(rt))
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:        toolSubmitCrawl,
		Description: "Submit an async batch crawl job; returns a task_id for get_crawl_job. Use for long-running crawls.",
	}, submitCrawlTool(rt))

	mcp.AddTool(s, &mcp.Tool{
		Name:        toolGetCrawlJob,
		Description: "Poll an async crawl job submitted with submit_crawl. Returns status (processing/completed/failed) and the crawl results once completed.",
	}, getCrawlJobTool(rt))

	mcp.AddTool(s, &mcp.Tool{
		Name:        toolCurrentTime,
		Description: "Get the current date and time on the server, in a chosen IANA time zone (default UTC). Use when the agent needs to know today's date, now, or timezone-converted times.",
	}, currentTimeTool())

	addSitemapTool(s)

	if err := addSearchTool(s, opts); err != nil {
		return nil, err
	}

	return s, nil
}

// ---------- inputs ----------

type mdIn struct {
	URL         string  `json:"url" jsonschema:"absolute http/https URL (or raw:/raw:// HTML)"`
	F           string  `json:"f,omitempty" jsonschema:"content filter: fit (default), raw, bm25 or llm"`
	Q           string  `json:"q,omitempty" jsonschema:"query for bm25/llm filters"`
	C           string  `json:"c,omitempty" jsonschema:"cache-bust counter"`
	Provider    string  `json:"provider,omitempty" jsonschema:"LLM provider for f=llm"`
	Temperature float64 `json:"temperature,omitempty" jsonschema:"LLM temperature for f=llm"`
}

type htmlIn struct {
	URL string `json:"url" jsonschema:"absolute http/https URL (or raw:/raw:// HTML)"`
}

type crawlIn struct {
	URLs           []string         `json:"urls" jsonschema:"1-100 URLs to crawl"`
	BrowserConfig  map[string]any   `json:"browser_config,omitempty" jsonschema:"Crawl4AI BrowserConfig object, forwarded as-is"`
	CrawlerConfig  map[string]any   `json:"crawler_config,omitempty" jsonschema:"Crawl4AI CrawlerRunConfig object, forwarded as-is"`
	CrawlerConfigs []map[string]any `json:"crawler_configs,omitempty" jsonschema:"per-URL CrawlerRunConfig objects (may carry url_matcher); takes precedence over crawler_config"`
}

type screenshotIn struct {
	URL               string   `json:"url" jsonschema:"absolute http/https URL"`
	ScreenshotWaitFor *float64 `json:"screenshot_wait_for,omitempty" jsonschema:"seconds to wait before capture (upstream default 2)"`
	WaitForImages     *bool    `json:"wait_for_images,omitempty" jsonschema:"wait for images to load"`
}

type pdfIn struct {
	URL string `json:"url" jsonschema:"absolute http/https URL"`
}

type executeJSIn struct {
	URL     string   `json:"url" jsonschema:"absolute http/https URL"`
	Scripts []string `json:"scripts" jsonschema:"JS snippets executed in order; each must be an expression returning a value"`
}

type askIn struct {
	Query       string  `json:"query" jsonschema:"BM25 search query; strongly recommended to limit response size"`
	ContextType string  `json:"context_type,omitempty" jsonschema:"code, doc or all (default all)"`
	ScoreRatio  float64 `json:"score_ratio,omitempty" jsonschema:"min score as fraction of max score, 0-1 (default 0.5)"`
	MaxResults  int     `json:"max_results,omitempty" jsonschema:"max chunks returned (default 20)"`
}

type submitCrawlIn struct {
	URLs          []string       `json:"urls" jsonschema:"URLs to crawl"`
	BrowserConfig map[string]any `json:"browser_config,omitempty"`
	CrawlerConfig map[string]any `json:"crawler_config,omitempty"`
}

type getCrawlJobIn struct {
	TaskID string `json:"task_id" jsonschema:"task id returned by submit_crawl"`
}

type currentTimeIn struct {
	Timezone string `json:"timezone,omitempty" jsonschema:"IANA time zone name, e.g. Asia/Tokyo, America/New_York; default UTC"`
}

// ---------- outputs ----------

type currentTimeOut struct {
	Timezone   string `json:"timezone"`
	Unix       int64  `json:"unix"`
	RFC3339    string `json:"rfc3339"`
	Human      string `json:"human"`
	Date       string `json:"date"`
	Time       string `json:"time"`
	Weekday    string `json:"weekday"`
	YearDay    int    `json:"day_of_year"`
	WeekNumber int    `json:"iso_week"`
}

type mdOut struct {
	Success  bool   `json:"success"`
	URL      string `json:"url"`
	Filter   string `json:"filter,omitempty"`
	Markdown string `json:"markdown,omitempty"`
}

type htmlOut struct {
	Success bool   `json:"success"`
	URL     string `json:"url"`
	HTML    string `json:"html,omitempty"`
}

type pageResult struct {
	URL      string         `json:"url"`
	Success  bool           `json:"success"`
	Markdown string         `json:"markdown,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Error    string         `json:"error,omitempty"`
	HTML     string         `json:"html,omitempty"`
}

type crawlOut struct {
	Success         bool         `json:"success"`
	FailedCount     int          `json:"failed_count,omitempty"`
	Pages           []pageResult `json:"pages"`
	ProcessingTimeS float64      `json:"server_processing_time_s,omitempty"`
	HTMLReturned    bool         `json:"html_returned,omitempty"`
}

type jobOut struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

type crawlJobOut struct {
	TaskID    string    `json:"task_id"`
	Status    string    `json:"status"`
	CreatedAt string    `json:"created_at,omitempty"`
	Error     string    `json:"error,omitempty"`
	Crawl     *crawlOut `json:"crawl,omitempty"`
}

// ---------- handlers ----------

func currentTimeTool() func(context.Context, *mcp.CallToolRequest, currentTimeIn) (*mcp.CallToolResult, currentTimeOut, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in currentTimeIn) (*mcp.CallToolResult, currentTimeOut, error) {
		loc := time.UTC
		if in.Timezone != "" && !strings.EqualFold(in.Timezone, "utc") {
			var err error
			loc, err = time.LoadLocation(in.Timezone)
			if err != nil {
				return nil, currentTimeOut{}, fmt.Errorf("unknown time zone %q: %w (use an IANA name like Asia/Tokyo)", in.Timezone, err)
			}
		}
		now := time.Now().In(loc)
		return nil, currentTimeOut{
			Timezone:   loc.String(),
			Unix:       now.Unix(),
			RFC3339:    now.Format(time.RFC3339),
			Human:      now.Format("Monday, 2006-01-02 15:04:05 MST"),
			Date:       now.Format("2006-01-02"),
			Time:       now.Format("15:04:05"),
			Weekday:    now.Weekday().String(),
			YearDay:    now.YearDay(),
			WeekNumber: isoWeek(now),
		}, nil
	}
}

// isoWeek returns the ISO 8601 week number of t.
func isoWeek(t time.Time) int {
	_, week := t.ISOWeek()
	return week
}

func mdTool(rt *router) func(context.Context, *mcp.CallToolRequest, mdIn) (*mcp.CallToolResult, mdOut, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in mdIn) (*mcp.CallToolResult, mdOut, error) {
		resp, err := rt.For(ctx, in.URL).Markdown(ctx, c4a.MDRequest{
			URL: in.URL, F: in.F, Q: in.Q, C: in.C, Provider: in.Provider, Temperature: in.Temperature,
		})
		if err != nil {
			return nil, mdOut{}, err
		}
		return nil, mdOut{
			Success: resp.Success, URL: resp.URL, Filter: resp.Filter, Markdown: resp.Markdown,
		}, nil
	}
}

func htmlTool(rt *router) func(context.Context, *mcp.CallToolRequest, htmlIn) (*mcp.CallToolResult, htmlOut, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in htmlIn) (*mcp.CallToolResult, htmlOut, error) {
		resp, err := rt.For(ctx, in.URL).HTML(ctx, in.URL)
		if err != nil {
			return nil, htmlOut{}, err
		}
		return nil, htmlOut{Success: resp.Success, URL: resp.URL, HTML: resp.HTML}, nil
	}
}

// summarizeCrawl converts a /crawl response into the agent-friendly shape:
// markdown + metadata per page, with per-page failures preserved.
func summarizeCrawl(resp *c4a.CrawlResponse, includeHTML bool) crawlOut {
	out := crawlOut{Success: true, Pages: make([]pageResult, 0, len(resp.Results))}
	for _, r := range resp.Results {
		p := pageResult{
			URL:      r.URL,
			Success:  r.Success,
			Markdown: c4a.MarkdownString(r.Markdown),
			Error:    firstNonEmpty(r.ErrorMessage, r.Error),
		}
		if includeHTML {
			p.HTML = r.CleanedHTML
			if p.HTML == "" {
				p.HTML = r.HTML
			}
		}
		if len(r.Metadata) > 0 {
			var md map[string]any
			if json.Unmarshal(r.Metadata, &md) == nil && md != nil {
				p.Metadata = md
			}
		}
		if !r.Success {
			out.FailedCount++
			if p.Error == "" {
				p.Error = "crawl failed"
			}
		}
		out.Pages = append(out.Pages, p)
	}
	out.Success = resp.Success && out.FailedCount == 0
	out.ProcessingTimeS = resp.ServerProcessingTimeS
	out.HTMLReturned = includeHTML
	return out
}

func crawlTool(rt *router) func(context.Context, *mcp.CallToolRequest, crawlIn) (*mcp.CallToolResult, crawlOut, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in crawlIn) (*mcp.CallToolResult, crawlOut, error) {
		if len(in.URLs) == 0 {
			return nil, crawlOut{}, fmt.Errorf("urls must not be empty")
		}
		// Respect a crawler_config html passthrough only via explicit flag below.
		resp, err := rt.crawl(ctx, c4a.CrawlRequest{
			URLs: in.URLs, BrowserConfig: in.BrowserConfig,
			CrawlerConfig: in.CrawlerConfig, CrawlerConfigs: in.CrawlerConfigs,
		})
		if err != nil {
			return nil, crawlOut{}, err
		}
		return nil, summarizeCrawl(resp, false), nil
	}
}

func screenshotTool(rt *router) func(context.Context, *mcp.CallToolRequest, screenshotIn) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in screenshotIn) (*mcp.CallToolResult, any, error) {
		resp, err := rt.For(ctx, in.URL).Screenshot(ctx, c4a.ScreenshotRequest{
			URL: in.URL, ScreenshotWaitFor: in.ScreenshotWaitFor, WaitForImages: in.WaitForImages,
		})
		if err != nil {
			return nil, any(nil), err
		}
		img, err := base64.StdEncoding.DecodeString(resp.Screenshot)
		if err != nil {
			return nil, any(nil), fmt.Errorf("decoding screenshot base64: %w", err)
		}
		res := &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.ImageContent{Data: img, MIMEType: "image/png"},
		}}
		// Artifact pointer (for re-download) rides as text alongside the image.
		if resp.ArtifactID != "" {
			res.Content = append(res.Content, &mcp.TextContent{
				Text: fmt.Sprintf("artifact_id: %s (%d bytes, fetch: GET %s)", resp.ArtifactID, resp.Size, resp.ArtifactURL),
			})
		}
		return res, any(nil), nil
	}
}

func pdfTool(rt *router) func(context.Context, *mcp.CallToolRequest, pdfIn) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in pdfIn) (*mcp.CallToolResult, any, error) {
		resp, err := rt.For(ctx, in.URL).PDF(ctx, in.URL)
		if err != nil {
			return nil, any(nil), err
		}
		blob, err := base64.StdEncoding.DecodeString(resp.PDF)
		if err != nil {
			return nil, any(nil), fmt.Errorf("decoding pdf base64: %w", err)
		}
		res := &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
				URI:      fmt.Sprintf("crawl4ai://pdf/%s", pick(resp.ArtifactID, "result.pdf")),
				MIMEType: "application/pdf",
				Blob:     blob,
			}},
		}}
		if resp.ArtifactID != "" {
			res.Content = append(res.Content, &mcp.TextContent{
				Text: fmt.Sprintf("artifact_id: %s (%d bytes, fetch: GET %s)", resp.ArtifactID, resp.Size, resp.ArtifactURL),
			})
		}
		return res, any(nil), nil
	}
}

func executeJSTool(rt *router) func(context.Context, *mcp.CallToolRequest, executeJSIn) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in executeJSIn) (*mcp.CallToolResult, any, error) {
		r, err := rt.For(ctx, in.URL).ExecuteJS(ctx, c4a.ExecuteJSRequest{URL: in.URL, Scripts: in.Scripts})
		if err != nil {
			return nil, any(nil), err
		}
		return nil, any(map[string]any{
			"success":             r.Success,
			"url":                 r.URL,
			"js_execution_result": r.JSExecution,
			"markdown":            c4a.MarkdownString(r.Markdown),
			"links":               r.Links,
			"error_message":       firstNonEmpty(r.ErrorMessage, r.Error),
		}), nil
	}
}

func askTool(rt *router) func(context.Context, *mcp.CallToolRequest, askIn) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in askIn) (*mcp.CallToolResult, any, error) {
		resp, err := rt.fallback().Ask(ctx, in.Query, in.ContextType, in.ScoreRatio, in.MaxResults)
		if err != nil {
			return nil, any(nil), err
		}
		var sb strings.Builder
		if resp.CodeContext != "" {
			sb.WriteString(resp.CodeContext)
		}
		if resp.DocContext != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n\n---\n\n")
			}
			sb.WriteString(resp.DocContext)
		}
		// With a query the upstream returns scored chunks instead of raw contexts.
		for _, chunk := range resp.CodeResults {
			appendChunk(&sb, "code", chunk)
		}
		for _, chunk := range resp.DocResults {
			appendChunk(&sb, "doc", chunk)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: sb.String()}}}, any(nil), nil
	}
}

func submitCrawlTool(rt *router) func(context.Context, *mcp.CallToolRequest, submitCrawlIn) (*mcp.CallToolResult, jobOut, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in submitCrawlIn) (*mcp.CallToolResult, jobOut, error) {
		id, err := rt.submit(ctx, c4a.CrawlJobRequest{
			URLs: in.URLs, BrowserConfig: in.BrowserConfig, CrawlerConfig: in.CrawlerConfig,
		})
		if err != nil {
			return nil, jobOut{}, err
		}
		return nil, jobOut{TaskID: id, Status: "processing"}, nil
	}
}

func getCrawlJobTool(rt *router) func(context.Context, *mcp.CallToolRequest, getCrawlJobIn) (*mcp.CallToolResult, crawlJobOut, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in getCrawlJobIn) (*mcp.CallToolResult, crawlJobOut, error) {
		st, err := rt.job(ctx, in.TaskID)
		if err != nil {
			return nil, crawlJobOut{}, err
		}
		out := crawlJobOut{TaskID: st.TaskID, Status: st.Status, CreatedAt: st.CreatedAt, Error: st.Error}
		if st.Status == "completed" && len(st.Result) > 0 {
			var cr c4a.CrawlResponse
			if err := json.Unmarshal(st.Result, &cr); err == nil {
				sum := summarizeCrawl(&cr, false)
				out.Crawl = &sum
			} else {
				out.Error = "completed but result could not be parsed: " + err.Error()
			}
		}
		return nil, out, nil
	}
}

// ---------- small helpers ----------

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func pick(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func appendChunk(sb *strings.Builder, kind string, c c4a.AskChunk) {
	if sb.Len() > 0 {
		sb.WriteString("\n\n")
	}
	fmt.Fprintf(sb, "[%s score=%v]\n", kind, c.Score)
	sb.WriteString(c.Text)
}
