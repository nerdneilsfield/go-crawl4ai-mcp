package c4a

import (
	"encoding/json"
	"testing"
)

func TestCrawlResultMarkdownUnion(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		want string
	}{
		{"string markdown", `{"url":"u","success":true,"markdown":"# hi"}`, "# hi"},
		{"object markdown", `{"url":"u","success":true,"markdown":{"raw_markdown":"# obj"}}`, "# obj"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r CrawlResult
			if err := json.Unmarshal([]byte(tc.json), &r); err != nil {
				t.Fatal(err)
			}
			if got := MarkdownString(r.Markdown); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestURLQueryEscape(t *testing.T) {
	if got := urlQueryEscape("a b+c&d"); got != "a%20b%2Bc%26d" {
		t.Fatalf("got %q", got)
	}
}
