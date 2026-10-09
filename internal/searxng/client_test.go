package searxng

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newStubServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, &Client{BaseURL: srv.URL}
}

func TestSearchParamsEncoding(t *testing.T) {
	var got url.Values
	_, cl := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write([]byte(`{"query":"x","results":[]}`))
	})
	if _, err := cl.Search(context.Background(), SearchRequest{
		Query: "点云配准 site:github.com", Language: "zh-CN",
		Categories: "news", TimeRange: "month", Page: 2,
	}); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"q":          "点云配准 site:github.com",
		"format":     "json",
		"language":   "zh-CN",
		"categories": "news",
		"time_range": "month",
		"pageno":     "2",
	} {
		if got.Get(k) != want {
			t.Fatalf("%s = %q, want %q", k, got.Get(k), want)
		}
	}
}

func TestSearchParsesResultsAndUnresponsive(t *testing.T) {
	// Upstream shape: unresponsive_engines is a list of [engine, error]
	// pairs (searx/webutils.py get_translated_errors).
	_, cl := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"query": "fpga",
			"results": [
				{"title": "Paper", "url": "https://example.org/p", "content": "An FPGA arch",
				 "engines": ["brave", "duckduckgo"], "score": 1.5, "publishedDate": "2026-01-01"}
			],
			"unresponsive_engines": [
				["brave", "too many requests"],
				["google", "CAPTCHA"]
			]
		}`))
	})
	resp, err := cl.Search(context.Background(), SearchRequest{Query: "fpga"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://example.org/p" {
		t.Fatalf("results: %+v", resp.Results)
	}
	if resp.Results[0].Snippet != "An FPGA arch" {
		t.Fatalf("snippet mapped from content: %+v", resp.Results[0])
	}
	if len(resp.UnresponsiveEngines) != 2 {
		t.Fatalf("unresponsive: %+v", resp.UnresponsiveEngines)
	}
	if resp.UnresponsiveEngines[0].Engine != "brave" || resp.UnresponsiveEngines[0].Error != "too many requests" {
		t.Fatalf("pair[0]: %+v", resp.UnresponsiveEngines[0])
	}
	if resp.UnresponsiveEngines[1].Engine != "google" || resp.UnresponsiveEngines[1].Error != "CAPTCHA" {
		t.Fatalf("pair[1]: %+v", resp.UnresponsiveEngines[1])
	}
	// Partial engine failure must not lose the results.
	if len(resp.Results) == 0 {
		t.Fatal("results dropped on partial engine failure")
	}
}

func TestSearchErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"403 json disabled", 403, "", "json format not enabled"},
		{"429 rate limited", 429, "slow down", "HTTP 429"},
		{"bad json", 200, "<html>not json</html>", "decoding response"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, cl := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := cl.Search(context.Background(), SearchRequest{Query: "x"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestSearchTimeout(t *testing.T) {
	_, cl := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	})
	cl.HC = &http.Client{Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, err := cl.Search(context.Background(), SearchRequest{Query: "x"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > time.Second {
		t.Fatal("timeout did not apply")
	}
}

func TestSearchValidation(t *testing.T) {
	_, cl := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not be called")
	})
	if _, err := cl.Search(context.Background(), SearchRequest{}); err == nil {
		t.Fatal("empty query must error")
	}
	if _, err := cl.Search(context.Background(), SearchRequest{Query: "x", TimeRange: "hour"}); err == nil {
		t.Fatal("invalid time_range must error")
	}
	_ = json.Marshal // keep import if unused elsewhere
}
