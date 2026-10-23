package server

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/appconfig"
)

const (
	defaultGeoSiteURL  = "https://github.com/MetaCubeX/meta-rules-dat/releases/latest/download/geosite.dat"
	defaultGeoIPURL    = "https://github.com/MetaCubeX/meta-rules-dat/releases/latest/download/geoip.dat"
	defaultGeoInterval = 24 * time.Hour
)

// geoStore holds the geosite/geoip categories the rules name and keeps them
// fresh: files are re-downloaded every interval, checked by parsing, and only
// then swapped in, so a bad download never replaces working data.
type geoStore struct {
	dir                string
	siteURL, ipURL     string
	wantSites, wantIPs map[string]bool // upper-case categories the rules use
	interval           time.Duration   // 0: never refresh
	sites              atomic.Pointer[map[string]*geoSet]
	ips                atomic.Pointer[map[string][]netip.Prefix]
}

func (g *geoStore) site(code string) *geoSet            { return (*g.sites.Load())[code] }
func (g *geoStore) prefixes(code string) []netip.Prefix { return (*g.ips.Load())[code] }

func (g *geoStore) loadSites(path string) error {
	m, err := loadGeoSite(path, g.wantSites)
	if err == nil {
		g.sites.Store(&m)
	}
	return err
}

func (g *geoStore) loadIPs(path string) error {
	m, err := loadGeoIP(path, g.wantIPs)
	if err == nil {
		g.ips.Store(&m)
	}
	return err
}

// newGeoStore loads the databases once. A missing file is downloaded (failure
// is fatal). An existing file older than interval is refreshed too, but if that
// fails the old copy is used, so an offline restart still works.
func newGeoStore(dir, siteURL, ipURL string, interval time.Duration, wantSites, wantIPs map[string]bool) (*geoStore, error) {
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("geo.dir not set and no user cache dir: %w", err)
		}
		dir = filepath.Join(base, "crawl4ai-mcp")
	}
	g := &geoStore{dir: dir, siteURL: siteURL, ipURL: ipURL, wantSites: wantSites, wantIPs: wantIPs, interval: interval}
	if len(wantSites) > 0 {
		if err := g.open("geosite.dat", siteURL, g.loadSites); err != nil {
			return nil, err
		}
	}
	if len(wantIPs) > 0 {
		if err := g.open("geoip.dat", ipURL, g.loadIPs); err != nil {
			return nil, err
		}
	}
	return g, nil
}

func (g *geoStore) open(name, url string, load func(string) error) error {
	path := filepath.Join(g.dir, name)
	fi, err := os.Stat(path)
	missing := err != nil
	if missing || (g.interval > 0 && time.Since(fi.ModTime()) > g.interval) {
		err := download(url, path, load)
		if err == nil {
			return nil
		}
		if missing {
			return err
		}
		appconfig.Logger.Warn("geo database refresh failed; using the existing file", zap.String("file", path), zap.Error(err))
	}
	return load(path)
}

// refresh re-downloads both databases. Each is replaced only if it downloads
// and parses; on failure the data already in use stays.
func (g *geoStore) refresh() error {
	var errs []error
	if len(g.wantSites) > 0 {
		errs = append(errs, download(g.siteURL, filepath.Join(g.dir, "geosite.dat"), g.loadSites))
	}
	if len(g.wantIPs) > 0 {
		errs = append(errs, download(g.ipURL, filepath.Join(g.dir, "geoip.dat"), g.loadIPs))
	}
	return errors.Join(errs...)
}

// run refreshes every interval until ctx ends.
func (g *geoStore) run(ctx context.Context) {
	if g.interval <= 0 {
		return
	}
	t := time.NewTicker(g.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := g.refresh(); err != nil {
				appconfig.Logger.Warn("geo database refresh failed; keeping the current data", zap.Error(err))
			} else {
				appconfig.Logger.Info("geo databases refreshed")
			}
		}
	}
}

// download fetches url to a temp file next to path, runs check on it, and only
// then renames it into place.
func download(url, path string, check func(path string) error) error {
	dir, name := filepath.Dir(path), filepath.Base(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	appconfig.Logger.Info("downloading geo database", zap.String("url", url), zap.String("to", path))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: %s: HTTP %d", name, url, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("downloading %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := check(tmp.Name()); err != nil {
		return fmt.Errorf("downloaded %s rejected: %w", name, err)
	}
	return os.Rename(tmp.Name(), path)
}

// Readers for the v2ray geosite.dat / geoip.dat files (what mihomo, Xray and
// sing-box all ship). Both are plain protobuf; the few fields we need are
// decoded by hand so the project takes no protobuf dependency.
//
//	GeoSiteList { repeated GeoSite entry = 1 }
//	GeoSite     { string country_code = 1; repeated Domain domain = 2 }
//	Domain      { Type type = 1 (0 keyword, 1 regex, 2 suffix, 3 exact); string value = 2 }
//	GeoIPList   { repeated GeoIP entry = 1 }
//	GeoIP       { string country_code = 1; repeated CIDR cidr = 2 }
//	CIDR        { bytes ip = 1; uint32 prefix = 2 }

// pbEach calls fn for every field in b. Varint fields arrive in v, length-
// delimited ones in data; fixed-width fields are skipped.
func pbEach(b []byte, fn func(num uint64, data []byte, v uint64) error) error {
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return fmt.Errorf("bad protobuf tag")
		}
		b = b[n:]
		num := tag >> 3
		switch tag & 7 {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return fmt.Errorf("bad protobuf varint")
			}
			b = b[n:]
			if err := fn(num, nil, v); err != nil {
				return err
			}
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return fmt.Errorf("bad protobuf length")
			}
			data := b[n : n+int(l)]
			b = b[n+int(l):]
			if err := fn(num, data, 0); err != nil {
				return err
			}
		case 1:
			if len(b) < 8 {
				return fmt.Errorf("truncated protobuf")
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return fmt.Errorf("truncated protobuf")
			}
			b = b[4:]
		default:
			return fmt.Errorf("unsupported protobuf wire type %d", tag&7)
		}
	}
	return nil
}

// geoEntries walks the top-level repeated entry field, calling fn with the
// upper-cased country_code and the entry body, only for codes in want.
func geoEntries(path string, want map[string]bool, fn func(code string, body []byte) error) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	err = pbEach(data, func(num uint64, entry []byte, _ uint64) error {
		if num != 1 {
			return nil
		}
		var code string
		_ = pbEach(entry, func(num uint64, d []byte, _ uint64) error {
			if num == 1 && code == "" {
				code = strings.ToUpper(string(d))
			}
			return nil
		})
		if !want[code] {
			return nil
		}
		found[code] = true
		return fn(code, entry)
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for c := range want {
		if !found[c] {
			return nil, fmt.Errorf("%s: no entry %q", path, c)
		}
	}
	return found, nil
}

// geoSet is one geosite category.
type geoSet struct {
	exact   map[string]bool
	suffix  map[string]bool // matches the domain itself and all subdomains
	keyword []string
	regex   []*regexp.Regexp
}

func (g *geoSet) match(host string) bool {
	if g.exact[host] {
		return true
	}
	for h := host; ; {
		if g.suffix[h] {
			return true
		}
		i := strings.IndexByte(h, '.')
		if i < 0 {
			break
		}
		h = h[i+1:]
	}
	for _, k := range g.keyword {
		if strings.Contains(host, k) {
			return true
		}
	}
	for _, re := range g.regex {
		if re.MatchString(host) {
			return true
		}
	}
	return false
}

// loadGeoSite loads only the wanted categories (upper-case codes).
func loadGeoSite(path string, want map[string]bool) (map[string]*geoSet, error) {
	sets := map[string]*geoSet{}
	_, err := geoEntries(path, want, func(code string, entry []byte) error {
		g := &geoSet{exact: map[string]bool{}, suffix: map[string]bool{}}
		err := pbEach(entry, func(num uint64, d []byte, _ uint64) error {
			if num != 2 {
				return nil
			}
			var typ uint64 // proto3 omits the zero value, which is "keyword"
			var val string
			if err := pbEach(d, func(num uint64, d []byte, v uint64) error {
				switch num {
				case 1:
					typ = v
				case 2:
					val = strings.ToLower(string(d))
				}
				return nil
			}); err != nil {
				return err
			}
			switch typ {
			case 0:
				g.keyword = append(g.keyword, val)
			case 1:
				re, err := regexp.Compile(val)
				if err != nil {
					return nil // v2ray's RE2 dialect is ours too; skip the odd one rather than refuse the whole file
				}
				g.regex = append(g.regex, re)
			case 2:
				g.suffix[val] = true
			case 3:
				g.exact[val] = true
			}
			return nil
		})
		sets[code] = g
		return err
	})
	return sets, err
}

// loadGeoIP loads only the wanted country codes (upper-case).
func loadGeoIP(path string, want map[string]bool) (map[string][]netip.Prefix, error) {
	sets := map[string][]netip.Prefix{}
	_, err := geoEntries(path, want, func(code string, entry []byte) error {
		return pbEach(entry, func(num uint64, d []byte, _ uint64) error {
			if num != 2 {
				return nil
			}
			var ip []byte
			var bits uint64
			if err := pbEach(d, func(num uint64, d []byte, v uint64) error {
				switch num {
				case 1:
					ip = d
				case 2:
					bits = v
				}
				return nil
			}); err != nil {
				return err
			}
			addr, ok := netip.AddrFromSlice(ip)
			if !ok {
				return fmt.Errorf("bad ip in %s", code)
			}
			p, err := addr.Prefix(int(bits))
			if err != nil {
				return err
			}
			sets[code] = append(sets[code], p)
			return nil
		})
	})
	return sets, err
}
