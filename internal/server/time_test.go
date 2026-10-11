package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func callCurrentTime(t *testing.T, tz string) currentTimeOut {
	t.Helper()
	handler := currentTimeTool()
	var in currentTimeIn
	if err := json.Unmarshal(mustJSON(t, currentTimeIn{Timezone: tz}), &in); err != nil {
		t.Fatal(err)
	}
	req := &mcp.CallToolRequest{}
	res, out, err := handler(context.Background(), req, in)
	if err != nil {
		t.Fatalf("tz=%q: %v", tz, err)
	}
	if res != nil {
		t.Fatalf("expected nil result, got %+v", res)
	}
	return out
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCurrentTimeDefaultsToUTC(t *testing.T) {
	got := callCurrentTime(t, "")
	if got.Timezone != "UTC" {
		t.Fatalf("timezone: %q", got.Timezone)
	}
	parsed, err := time.Parse(time.RFC3339, got.RFC3339)
	if err != nil {
		t.Fatalf("rfc3339 %q: %v", got.RFC3339, err)
	}
	if _, offset := parsed.Zone(); offset != 0 {
		t.Fatalf("expected UTC offset 0, got %d", offset)
	}
	if got.Human == "" || got.Date == "" || got.Time == "" || got.Weekday == "" {
		t.Fatalf("empty fields: %+v", got)
	}
	if got.WeekNumber < 1 || got.WeekNumber > 53 {
		t.Fatalf("iso week out of range: %d", got.WeekNumber)
	}
	if got.YearDay < 1 || got.YearDay > 366 {
		t.Fatalf("year day out of range: %d", got.YearDay)
	}
	// Unix and RFC3339 must refer to roughly the same instant.
	if now := time.Now().Unix(); abs64(got.Unix-now) > 1 {
		t.Fatalf("unix %d too far from now %d", got.Unix, now)
	}
}

func TestCurrentTimeWithTimezone(t *testing.T) {
	got := callCurrentTime(t, "Asia/Tokyo")
	if got.Timezone != "Asia/Tokyo" {
		t.Fatalf("timezone: %q", got.Timezone)
	}
	parsed, err := time.Parse(time.RFC3339, got.RFC3339)
	if err != nil {
		t.Fatal(err)
	}
	if _, offset := parsed.Zone(); offset != 9*3600 {
		t.Fatalf("expected +09:00, got offset %d", offset)
	}
	// Tokyo and UTC results must be the same instant.
	utc := callCurrentTime(t, "")
	a, _ := time.Parse(time.RFC3339, got.RFC3339)
	b, _ := time.Parse(time.RFC3339, utc.RFC3339)
	if abs64(a.Unix()-b.Unix()) > 1 {
		t.Fatalf("tokyo %v and utc %v disagree", got.RFC3339, utc.RFC3339)
	}
}

func TestCurrentTimeInvalidTimezone(t *testing.T) {
	handler := currentTimeTool()
	var in currentTimeIn
	if err := json.Unmarshal([]byte(`{"timezone":"Mars/Olympus"}`), &in); err != nil {
		t.Fatal(err)
	}
	if _, _, err := handler(context.Background(), &mcp.CallToolRequest{}, in); err == nil {
		t.Fatal("expected error for invalid timezone")
	} else if !stringsContains(err.Error(), "unknown time zone") {
		t.Fatalf("error: %v", err)
	}
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func stringsContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
