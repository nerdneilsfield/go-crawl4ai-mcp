package server

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/appconfig"
)

const (
	defaultGeoSiteURL = "https://github.com/MetaCubeX/meta-rules-dat/releases/latest/download/geosite.dat"
	defaultGeoIPURL   = "https://github.com/MetaCubeX/meta-rules-dat/releases/latest/download/geoip.dat"
)

// ensureGeoFile returns dir/name, downloading it from url first if it is not
// there. An existing file is never refreshed; delete it to update.
func ensureGeoFile(dir, name, url string) (string, error) {
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("geo.dir not set and no user cache dir: %w", err)
		}
		dir = filepath.Join(base, "crawl4ai-mcp")
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	appconfig.Logger.Info("downloading geo database", zap.String("url", url), zap.String("to", path))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: %s: HTTP %d", name, url, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return "", fmt.Errorf("downloading %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
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
