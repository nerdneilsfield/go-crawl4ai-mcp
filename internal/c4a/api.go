package c4a

import (
	"context"
	"encoding/json"
)

// ---------- shared request/response shapes (from deploy/docker/schemas.py) ----------

// CrawlResult is one per-URL result inside a /crawl (or execute_js) response.
// Heavy optional payloads are kept as raw JSON and only surfaced on demand.
type CrawlResult struct {
	URL             string          `json:"url"`
	Success         bool            `json:"success"`
	ErrorMessage    string          `json:"error_message,omitempty"`
	Markdown        json.RawMessage `json:"markdown,omitempty"` // string or {raw_markdown, markdown_with_citation_links, ...}
	HTML            string          `json:"html,omitempty"`
	CleanedHTML     string          `json:"cleaned_html,omitempty"`
	Extracted       string          `json:"extracted_content,omitempty"`
	Media           json.RawMessage `json:"media,omitempty"`
	Links           json.RawMessage `json:"links,omitempty"`
	Metadata        json.RawMessage `json:"metadata,omitempty"`
	Screenshot      string          `json:"screenshot,omitempty"` // base64 PNG
	PDF             string          `json:"pdf,omitempty"`        // base64 PDF
	MHTML           string          `json:"mhtml,omitempty"`
	JSExecution     json.RawMessage `json:"js_execution_result,omitempty"`
	DownloadedFiles []string        `json:"downloaded_files,omitempty"`
	ActionErrors    json.RawMessage `json:"action_errors,omitempty"`
	StatusCode      int             `json:"status_code,omitempty"`
	Error           string          `json:"error,omitempty"`
}

// ---------- /md ----------

type MDRequest struct {
	URL         string  `json:"url"`
	F           string  `json:"f,omitempty"`           // raw | fit (default) | bm25 | llm
	Q           string  `json:"q,omitempty"`           // query for bm25/llm filters
	C           string  `json:"c,omitempty"`           // cache-bust counter
	Provider    string  `json:"provider,omitempty"`    // llm filter only
	Temperature float64 `json:"temperature,omitempty"` // llm filter only
}

type MDResponse struct {
	URL      string `json:"url"`
	Filter   string `json:"filter"`
	Query    string `json:"query"`
	Cache    string `json:"cache"`
	Markdown string `json:"markdown"`
	Success  bool   `json:"success"`
}

func (c *Client) Markdown(ctx context.Context, req MDRequest) (*MDResponse, error) {
	var out MDResponse
	if err := c.do(ctx, "POST", "/md", &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------- /html ----------

type HTMLResponse struct {
	HTML    string `json:"html"`
	URL     string `json:"url"`
	Success bool   `json:"success"`
}

func (c *Client) HTML(ctx context.Context, url string) (*HTMLResponse, error) {
	var out HTMLResponse
	if err := c.do(ctx, "POST", "/html", map[string]string{"url": url}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------- /crawl ----------

type CrawlRequest struct {
	URLs           []string         `json:"urls"`
	BrowserConfig  map[string]any   `json:"browser_config,omitempty"`
	CrawlerConfig  map[string]any   `json:"crawler_config,omitempty"`
	CrawlerConfigs []map[string]any `json:"crawler_configs,omitempty"`
}

type CrawlResponse struct {
	Success               bool          `json:"success"`
	Results               []CrawlResult `json:"results"`
	ServerProcessingTimeS float64       `json:"server_processing_time_s,omitempty"`
}

func (c *Client) Crawl(ctx context.Context, req CrawlRequest) (*CrawlResponse, error) {
	var out CrawlResponse
	if err := c.do(ctx, "POST", "/crawl", &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------- /screenshot ----------

type ScreenshotRequest struct {
	URL               string   `json:"url"`
	ScreenshotWaitFor *float64 `json:"screenshot_wait_for,omitempty"`
	WaitForImages     *bool    `json:"wait_for_images,omitempty"`
}

type ScreenshotResponse struct {
	Success     bool   `json:"success"`
	Screenshot  string `json:"screenshot"` // base64 PNG
	ArtifactID  string `json:"artifact_id,omitempty"`
	ArtifactURL string `json:"url,omitempty"` // "/artifacts/{id}"
	Mime        string `json:"mime,omitempty"`
	Size        int64  `json:"size,omitempty"`
}

func (c *Client) Screenshot(ctx context.Context, req ScreenshotRequest) (*ScreenshotResponse, error) {
	var out ScreenshotResponse
	if err := c.do(ctx, "POST", "/screenshot", &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------- /pdf ----------

type PDFRequest struct {
	URL string `json:"url"`
}

type PDFResponse struct {
	Success     bool   `json:"success"`
	PDF         string `json:"pdf"` // base64 PDF
	ArtifactID  string `json:"artifact_id,omitempty"`
	ArtifactURL string `json:"url,omitempty"`
	Mime        string `json:"mime,omitempty"`
	Size        int64  `json:"size,omitempty"`
}

func (c *Client) PDF(ctx context.Context, url string) (*PDFResponse, error) {
	var out PDFResponse
	if err := c.do(ctx, "POST", "/pdf", map[string]string{"url": url}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------- /execute_js ----------

type ExecuteJSRequest struct {
	URL     string   `json:"url"`
	Scripts []string `json:"scripts"`
}

func (c *Client) ExecuteJS(ctx context.Context, req ExecuteJSRequest) (*CrawlResult, error) {
	var out CrawlResult
	if err := c.do(ctx, "POST", "/execute_js", &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------- GET /ask ----------

type AskResponse struct {
	CodeContext string `json:"code_context,omitempty"`
	DocContext  string `json:"doc_context,omitempty"`
}

func (c *Client) Ask(ctx context.Context, query, contextType string, scoreRatio float64, maxResults int) (*AskResponse, error) {
	if contextType == "" {
		contextType = "all"
	}
	q := map[string]string{
		"context_type": contextType,
	}
	if query != "" {
		q["query"] = query
	}
	if scoreRatio > 0 {
		q["score_ratio"] = json.Number(fmtFloat(scoreRatio)).String()
	}
	if maxResults > 0 {
		q["max_results"] = fmtInt(maxResults)
	}
	var out AskResponse
	if err := c.get(ctx, "/ask", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------- /crawl/job ----------

type CrawlJobRequest struct {
	URLs          []string       `json:"urls"`
	BrowserConfig map[string]any `json:"browser_config,omitempty"`
	CrawlerConfig map[string]any `json:"crawler_config,omitempty"`
}

func (c *Client) SubmitCrawlJob(ctx context.Context, req CrawlJobRequest) (taskID string, err error) {
	var out struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := c.do(ctx, "POST", "/crawl/job", &req, &out); err != nil {
		return "", err
	}
	return out.TaskID, nil
}

// CrawlJobStatus is the /crawl/job/{task_id} response. Result is the same
// shape as a /crawl response once the job completes.
type CrawlJobStatus struct {
	TaskID    string          `json:"task_id"`
	Status    string          `json:"status"` // processing | completed | failed
	CreatedAt string          `json:"created_at"`
	URL       string          `json:"url"`
	Result    json.RawMessage `json:"result,omitempty"` // CrawlResponse JSON when completed
	Error     string          `json:"error,omitempty"`
}

func (c *Client) CrawlJob(ctx context.Context, taskID string) (*CrawlJobStatus, error) {
	var out CrawlJobStatus
	if err := c.get(ctx, "/crawl/job/"+taskID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
