package server

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func pbVarint(num, v uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, num<<3), v)
}
func pbBytes(num uint64, b []byte) []byte {
	out := binary.AppendUvarint(binary.AppendUvarint(nil, num<<3|2), uint64(len(b)))
	return append(out, b...)
}
func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// geosite.dat / geoip.dat encoded per v2ray's protobuf schema; keyword is type 0,
// which proto3 leaves off the wire.
func TestGeoDatRouting(t *testing.T) {
	dom := func(typ uint64, v string) []byte {
		d := pbBytes(2, []byte(v))
		if typ != 0 {
			d = cat(pbVarint(1, typ), d)
		}
		return pbBytes(2, d)
	}
	site := pbBytes(1, cat(pbBytes(1, []byte("CN")), dom(0, "baidu"), dom(1, `^img\d+\.cdn\.com$`), dom(2, "bilibili.com"), dom(3, "exact.org")))
	other := pbBytes(1, cat(pbBytes(1, []byte("GOOGLE")), dom(2, "google.com")))
	cidr := func(ip []byte, bits uint64) []byte { return pbBytes(2, cat(pbBytes(1, ip), pbVarint(2, bits))) }
	ip := pbBytes(1, cat(pbBytes(1, []byte("CN")), cidr([]byte{114, 114, 0, 0}, 16)))

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "geosite.dat"), cat(site, other), 0o600)
	os.WriteFile(filepath.Join(dir, "geoip.dat"), ip, 0o600)
	cfg := filepath.Join(dir, "r.yaml")
	os.WriteFile(cfg, []byte("backends: {hk: {url: http://hk}, us: {url: http://us}}\n"+
		"geo: {dir: "+dir+"}\n"+
		"rules: [\"GEOSITE,cn,hk\", \"GEOIP,cn,hk\", \"MATCH,us\"]\n"), 0o600)
	rt, err := loadRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for u, want := range map[string]string{
		"https://www.baidu.com":  "hk", // keyword
		"https://img7.cdn.com":   "hk", // regex
		"https://m.bilibili.com": "hk", // suffix
		"https://exact.org":      "hk",
		"https://sub.exact.org":  "us", // exact does not cover subdomains
		"https://google.com":     "us", // category not selected by any rule
		"http://114.114.8.8/":    "hk",
		"http://114.115.0.1/":    "us",
	} {
		if got := rt.pick(context.Background(), u); got != want {
			t.Errorf("pick(%s) = %s, want %s", u, got, want)
		}
	}
}

func siteDat(domains ...string) []byte {
	var d []byte
	for _, v := range domains {
		d = append(d, pbBytes(2, cat(pbVarint(1, 3), pbBytes(2, []byte(v))))...)
	}
	return pbBytes(1, cat(pbBytes(1, []byte("CN")), d))
}

func geoCfg(dir, url, extra string) string {
	return "backends: {hk: {url: http://hk}, us: {url: http://us}}\n" +
		"geo: {dir: " + dir + ", geosite_url: " + url + extra + "}\n" +
		"rules: [\"GEOSITE,cn,hk\", \"MATCH,us\"]\n"
}

// A missing database is fetched once and reused; an old one is refreshed on
// start, but if that fails the old copy still serves.
func TestGeoDownloadAndStartupRefresh(t *testing.T) {
	hits, fail := 0, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if fail {
			http.Error(w, "down", 500)
			return
		}
		w.Write(siteDat("a.cn"))
	}))
	defer srv.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "r.yaml")
	os.WriteFile(cfg, []byte(geoCfg(dir+"/db", srv.URL, "")), 0o600)
	load := func() *router {
		rt, err := loadRouter(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return rt
	}
	ctx := context.Background()
	if got := load().pick(ctx, "https://a.cn"); got != "hk" || hits != 1 {
		t.Fatalf("first start: pick=%s hits=%d", got, hits)
	}
	load()
	if hits != 1 {
		t.Errorf("fresh file was re-downloaded (hits=%d)", hits)
	}
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(dir+"/db/geosite.dat", old, old)
	load()
	if hits != 2 {
		t.Errorf("stale file was not refreshed on start (hits=%d)", hits)
	}
	os.Chtimes(dir+"/db/geosite.dat", old, old)
	fail = true
	if got := load().pick(ctx, "https://a.cn"); got != "hk" {
		t.Errorf("offline restart must fall back to the old file, got %s", got)
	}
}

// refresh swaps new data into a live router, and a bad download (error page,
// truncated file) leaves the working data alone.
func TestGeoRefreshHotSwapAndRejectsBad(t *testing.T) {
	body := siteDat("a.cn")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(body) }))
	defer srv.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "r.yaml")
	os.WriteFile(cfg, []byte(geoCfg(dir+"/db", srv.URL, "")), 0o600)
	rt, err := loadRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if rt.pick(ctx, "https://b.cn") != "us" {
		t.Fatal("b.cn should not match yet")
	}

	body = siteDat("a.cn", "b.cn")
	if err := rt.geo.refresh(); err != nil {
		t.Fatal(err)
	}
	if rt.pick(ctx, "https://b.cn") != "hk" {
		t.Error("refreshed data not live")
	}

	for name, bad := range map[string][]byte{
		"html error page": []byte("<html>503 Service Unavailable</html>"),
		"truncated":       siteDat("c.cn")[:6],
		"wrong category":  pbBytes(1, cat(pbBytes(1, []byte("JP")))),
	} {
		body = bad
		if err := rt.geo.refresh(); err == nil {
			t.Errorf("%s: refresh should fail", name)
		}
		if rt.pick(ctx, "https://b.cn") != "hk" || rt.pick(ctx, "https://a.cn") != "hk" {
			t.Errorf("%s: good data was lost", name)
		}
	}
	left, _ := filepath.Glob(dir + "/db/geosite.dat.*")
	if len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

func TestGeoUpdateIntervalConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(siteDat("a.cn")) }))
	defer srv.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "r.yaml")
	for extra, want := range map[string]time.Duration{"": 24 * time.Hour, ", update_interval: 6h": 6 * time.Hour, ", update_interval: 0s": 0} {
		os.WriteFile(cfg, []byte(geoCfg(dir+"/db", srv.URL, extra)), 0o600)
		rt, err := loadRouter(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if rt.geo.interval != want {
			t.Errorf("%q: interval = %v, want %v", extra, rt.geo.interval, want)
		}
	}
	os.WriteFile(cfg, []byte(geoCfg(dir+"/db", srv.URL, ", update_interval: -1h")), 0o600)
	if _, err := loadRouter(cfg); err == nil {
		t.Error("negative interval must be rejected")
	}
}

// The background loop picks up new data on its own and stops with its context.
func TestGeoRunRefreshesUntilCancelled(t *testing.T) {
	var body atomic.Pointer[[]byte]
	set := func(b []byte) { body.Store(&b) }
	set(siteDat("a.cn"))
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Write(*body.Load())
	}))
	defer srv.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "r.yaml")
	os.WriteFile(cfg, []byte(geoCfg(dir+"/db", srv.URL, ", update_interval: 100ms")), 0o600)
	rt, err := loadRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rt.geo.run(ctx); close(done) }()
	set(siteDat("a.cn", "late.cn"))
	deadline := time.Now().Add(3 * time.Second)
	for rt.pick(context.Background(), "https://late.cn") != "hk" {
		if time.Now().After(deadline) {
			t.Fatal("background refresh never applied")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not stop after cancel")
	}
}
