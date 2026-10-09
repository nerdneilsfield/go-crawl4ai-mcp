// Package c4a is a minimal REST client for a self-hosted Crawl4AI server
// (deploy/docker/server.py), mapping the HTTP endpoints this MCP server needs.
package c4a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client talks to a Crawl4AI Docker server's REST API.
type Client struct {
	BaseURL string // e.g. "http://127.0.0.1:11235"
	Token   string // CRAWL4AI_API_TOKEN; empty means no auth (loopback default)
	HC      *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HC != nil {
		return c.HC
	}
	return http.DefaultClient
}

// apiError is a non-2xx upstream response.
type apiError struct {
	StatusCode int
	Detail     string
}

func (e *apiError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("crawl4ai: HTTP %d: %s", e.StatusCode, e.Detail)
	}
	return fmt.Sprintf("crawl4ai: HTTP %d", e.StatusCode)
}

// IsNotFound reports whether err is an upstream 404.
func IsNotFound(err error) bool {
	ae, ok := err.(*apiError)
	return ok && ae.StatusCode == http.StatusNotFound
}

// do sends a JSON request and decodes the JSON response into out (if non-nil).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20)) // 256 MiB cap: batch crawls embed base64 PDFs/screenshots
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ae := &apiError{StatusCode: resp.StatusCode}
		var detail struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(data, &detail) == nil && detail.Detail != "" {
			ae.Detail = detail.Detail
		} else {
			ae.Detail = strings.TrimSpace(string(data))
			if len(ae.Detail) > 500 {
				ae.Detail = ae.Detail[:500]
			}
		}
		return ae
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decoding crawl4ai response from %s: %w", path, err)
	}
	return nil
}

// MarkdownString normalizes CrawlResult.markdown, which is a JSON string on
// /md but an object ({raw_markdown, ...}) on /crawl and execute_js results.
func MarkdownString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		RawMarkdown           string `json:"raw_markdown"`
		MarkdownWithCitations string `json:"markdown_with_citations"`
		FitMarkdown           string `json:"fit_markdown"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if obj.RawMarkdown != "" {
			return obj.RawMarkdown
		}
		if obj.FitMarkdown != "" {
			return obj.FitMarkdown
		}
		return obj.MarkdownWithCitations
	}
	return ""
}

func (c *Client) get(ctx context.Context, path string, query map[string]string, out any) error {
	if len(query) > 0 {
		var sb strings.Builder
		sb.WriteString(path)
		first := true
		for k, v := range query {
			sb.WriteString(map[bool]string{true: "?", false: "&"}[first])
			first = false
			sb.WriteString(k)
			sb.WriteString("=")
			sb.WriteString(urlQueryEscape(v))
		}
		path = sb.String()
	}
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func urlQueryEscape(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') ||
			ch == '-' || ch == '_' || ch == '.' || ch == '~' {
			sb.WriteByte(ch)
		} else {
			fmt.Fprintf(&sb, "%%%02X", ch)
		}
	}
	return sb.String()
}
