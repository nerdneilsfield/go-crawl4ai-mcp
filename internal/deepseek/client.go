// Package deepseek is a minimal client for the DeepSeek Search API
// (POST /v1/tools/search). Billing is per successful request, so the client
// does no retrying or caching of its own.
package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/appconfig"
)

const defaultBaseURL = "https://api.deepseek.com"

// Client talks to the DeepSeek Search API.
type Client struct {
	BaseURL string // default "https://api.deepseek.com"
	APIKey  string
	HC      *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HC != nil {
		return c.HC
	}
	// Shared client: search must fail fast, independent of crawl timeouts.
	return appconfig.SearchHTTPClient
}

// SearchRequest carries the request fields this client supports. Query
// operators (site:, -site:, lang:, after:, before:, quotes) pass through as-is.
// need_content is never sent: it defaults to false, so no full text is fetched.
type SearchRequest struct {
	Query       string `json:"query"`                  // required, 1-128 chars
	Count       int    `json:"count,omitempty"`        // 1-50; API default 10 when 0
	OriQuestion string `json:"ori_question,omitempty"` // original user question, helps ranking
}

// SearchResult is one hit.
type SearchResult struct {
	RerankScore float64 `json:"rerank_score"` // 0-3; >=1.5 is high quality
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Snippet     string  `json:"snippet"`
	Summary     string  `json:"summary"` // long summary meant for the model to read
	SiteName    string  `json:"site_name"`
	SiteIcon    string  `json:"site_icon"`
	Date        string  `json:"date"`      // RFC3339 when known
	Authority   int     `json:"authority"` // 0-4, -1 unknown
}

// SearchResponse is the API payload.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
}

// Error is a non-2xx upstream response.
type Error struct {
	StatusCode int
	Message    string
}

func (e *Error) Error() string {
	msg := e.Message
	if e.StatusCode == http.StatusPaymentRequired {
		msg = "insufficient balance, top up the DeepSeek account: " + msg
	}
	if msg != "" {
		return fmt.Sprintf("deepseek: HTTP %d: %s", e.StatusCode, msg)
	}
	return fmt.Sprintf("deepseek: HTTP %d", e.StatusCode)
}

// Search runs a query.
func (c *Client) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	if strings.TrimSpace(req.Query) == "" {
		return nil, fmt.Errorf("deepseek: empty query")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/tools/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = strings.TrimSpace(string(data))
			if len(msg) > 300 {
				msg = msg[:300]
			}
		}
		return nil, &Error{StatusCode: resp.StatusCode, Message: msg}
	}

	var out SearchResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("deepseek: decoding response: %w", err)
	}
	return &out, nil
}
