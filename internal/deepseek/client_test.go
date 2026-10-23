package deepseek

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newStubServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, APIKey: "sk-test"}
}

func TestSearchRequestAndParse(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotPath, gotMethod string
	cl := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotMethod = r.Header.Get("Authorization"), r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"results":[{
			"rerank_score": 2.5, "title": "Paper", "url": "https://example.org/p",
			"snippet": "short", "summary": "long summary", "content": "",
			"site_name": "Example", "site_icon": null,
			"date": "2026-01-02T03:04:05Z", "authority": 3}]}`))
	})
	resp, err := cl.Search(context.Background(), SearchRequest{Query: `fpga site:arxiv.org lang:en`, Count: 5, OriQuestion: "what is fpga"})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/tools/search" || gotAuth != "Bearer sk-test" {
		t.Fatalf("request = %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}
	if gotBody["query"] != `fpga site:arxiv.org lang:en` || gotBody["count"] != float64(5) || gotBody["ori_question"] != "what is fpga" {
		t.Fatalf("body = %v", gotBody)
	}
	if _, ok := gotBody["need_content"]; ok {
		t.Fatalf("need_content must not be sent: %v", gotBody)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("results = %d", len(resp.Results))
	}
	r := resp.Results[0]
	if r.RerankScore != 2.5 || r.Authority != 3 || r.Date != "2026-01-02T03:04:05Z" ||
		r.Summary != "long summary" || r.Snippet != "short" || r.SiteName != "Example" || r.SiteIcon != "" {
		t.Fatalf("parsed = %+v", r)
	}
}

func TestSearchErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   []string
	}{
		{http.StatusPaymentRequired, []string{"HTTP 402", "insufficient balance", "no money"}},
		{http.StatusTooManyRequests, []string{"HTTP 429", "no money"}},
	} {
		cl := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":{"message":"no money","type":"x","param":null,"code":"y"}}`))
		})
		_, err := cl.Search(context.Background(), SearchRequest{Query: "q"})
		if err == nil {
			t.Fatalf("status %d: expected error", tc.status)
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Fatalf("status %d: error %q missing %q", tc.status, err, w)
			}
		}
	}
}

func TestSearchEmptyResults(t *testing.T) {
	cl := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	resp, err := cl.Search(context.Background(), SearchRequest{Query: "q"})
	if err != nil || len(resp.Results) != 0 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	if _, err := (&Client{}).Search(context.Background(), SearchRequest{Query: "  "}); err == nil {
		t.Fatal("expected error for empty query")
	}
}
