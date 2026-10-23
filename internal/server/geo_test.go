package server

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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

// A missing database is fetched from geo.geosite_url into geo.dir once, then reused.
func TestGeoDownloadOnce(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Write(pbBytes(1, cat(pbBytes(1, []byte("CN")), pbBytes(2, cat(pbVarint(1, 3), pbBytes(2, []byte("a.cn")))))))
	}))
	defer srv.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "r.yaml")
	os.WriteFile(cfg, []byte("backends: {hk: {url: http://hk}, us: {url: http://us}}\n"+
		"geo: {dir: "+dir+"/db, geosite_url: "+srv.URL+"}\n"+
		"rules: [\"GEOSITE,cn,hk\", \"MATCH,us\"]\n"), 0o600)
	for range 2 {
		rt, err := loadRouter(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := rt.pick(context.Background(), "https://a.cn"); got != "hk" {
			t.Fatalf("pick = %s", got)
		}
	}
	if hits != 1 {
		t.Errorf("downloaded %d times, want 1", hits)
	}
}
