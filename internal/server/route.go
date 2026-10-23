package server

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/appconfig"
	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/c4a"
)

// router sends each tool call to one of several Crawl4AI backends, choosing by
// the target URL with mihomo-style rules (first match wins). The last rule is
// always MATCH, so every URL has an answer.
type router struct {
	clients map[string]*c4a.Client
	rules   []rule
	dns     *resolver // nil unless dns.enable: IP-CIDR/GEOIP then only see literal IPs
}

type rule struct {
	kind    string // DOMAIN | DOMAIN-SUFFIX | DOMAIN-KEYWORD | IP-CIDR | GEOSITE | GEOIP | MATCH
	val     string
	cidrs   []netip.Prefix // IP-CIDR: one prefix; GEOIP: the country's prefixes
	site    *geoSet        // GEOSITE
	backend string
}

// singleRouter is the no-config case: one backend, everything goes to it.
func singleRouter(cl *c4a.Client) *router {
	return &router{
		clients: map[string]*c4a.Client{"default": cl},
		rules:   []rule{{kind: "MATCH", backend: "default"}},
	}
}

type routeFile struct {
	Backends map[string]struct {
		URL   string `yaml:"url"`
		Token string `yaml:"token"`
	} `yaml:"backends"`
	Geo struct {
		Dir        string `yaml:"dir"`         // where geosite.dat / geoip.dat live; default: user cache dir
		GeoSiteURL string `yaml:"geosite_url"` // downloaded into dir when the file is missing
		GeoIPURL   string `yaml:"geoip_url"`
	} `yaml:"geo"`
	DNS struct {
		Enable  bool     `yaml:"enable"`
		Servers []string `yaml:"servers"` // https://... (DoH) or tls://host[:853] (DoT)
	} `yaml:"dns"`
	Rules []string `yaml:"rules"`
}

func loadRouter(path string) (*router, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f routeFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	r := &router{clients: map[string]*c4a.Client{}}
	for name, b := range f.Backends {
		if name == "" || strings.Contains(name, ":") { // ':' separates backend from task id
			return nil, fmt.Errorf("%s: invalid backend name %q", path, name)
		}
		if b.URL == "" {
			return nil, fmt.Errorf("%s: backend %q has no url", path, name)
		}
		r.clients[name] = &c4a.Client{BaseURL: strings.TrimRight(b.URL, "/"), Token: b.Token}
	}
	if len(r.clients) == 0 {
		return nil, fmt.Errorf("%s: no backends", path)
	}
	for i, line := range f.Rules {
		ru, err := parseRule(line)
		if err != nil {
			return nil, fmt.Errorf("%s: rule %q: %w", path, line, err)
		}
		if r.clients[ru.backend] == nil {
			return nil, fmt.Errorf("%s: rule %q: unknown backend %q", path, line, ru.backend)
		}
		if (ru.kind == "MATCH") != (i == len(f.Rules)-1) {
			return nil, fmt.Errorf("%s: MATCH must be the last rule, and the last rule must be MATCH", path)
		}
		r.rules = append(r.rules, ru)
	}
	if len(r.rules) == 0 {
		return nil, fmt.Errorf("%s: no rules", path)
	}
	if err := r.attachGeo(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.DNS.Enable {
		if r.dns, err = newResolver(f.DNS.Servers); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return r, nil
}

// parseRule parses "TYPE,value,backend" or "MATCH,backend".
func parseRule(line string) (rule, error) {
	p := strings.Split(line, ",")
	for i := range p {
		p[i] = strings.TrimSpace(p[i])
	}
	kind := strings.ToUpper(p[0])
	if kind == "MATCH" {
		if len(p) != 2 {
			return rule{}, fmt.Errorf("want MATCH,backend")
		}
		return rule{kind: kind, backend: p[1]}, nil
	}
	if len(p) != 3 || p[1] == "" {
		return rule{}, fmt.Errorf("want TYPE,value,backend")
	}
	ru := rule{kind: kind, val: strings.ToLower(p[1]), backend: p[2]}
	switch kind {
	case "DOMAIN", "DOMAIN-SUFFIX", "DOMAIN-KEYWORD":
	case "GEOSITE", "GEOIP":
		ru.val = strings.ToUpper(p[1]) // resolved against the database in attachGeo
	case "IP-CIDR":
		pf, err := netip.ParsePrefix(p[1])
		if err != nil {
			return rule{}, err
		}
		ru.cidrs = []netip.Prefix{pf.Masked()}
	default:
		return rule{}, fmt.Errorf("unknown rule type %q", p[0])
	}
	return ru, nil
}

// attachGeo loads, from the database files, just the categories the rules name.
func (r *router) attachGeo(f *routeFile) error {
	sites, ips := map[string]bool{}, map[string]bool{}
	for _, ru := range r.rules {
		switch ru.kind {
		case "GEOSITE":
			sites[ru.val] = true
		case "GEOIP":
			ips[ru.val] = true
		}
	}
	var siteSets map[string]*geoSet
	var ipSets map[string][]netip.Prefix
	if len(sites) > 0 {
		path, err := ensureGeoFile(f.Geo.Dir, "geosite.dat", pick(f.Geo.GeoSiteURL, defaultGeoSiteURL))
		if err != nil {
			return err
		}
		if siteSets, err = loadGeoSite(path, sites); err != nil {
			return err
		}
	}
	if len(ips) > 0 {
		path, err := ensureGeoFile(f.Geo.Dir, "geoip.dat", pick(f.Geo.GeoIPURL, defaultGeoIPURL))
		if err != nil {
			return err
		}
		if ipSets, err = loadGeoIP(path, ips); err != nil {
			return err
		}
	}
	for i := range r.rules {
		switch r.rules[i].kind {
		case "GEOSITE":
			r.rules[i].site = siteSets[r.rules[i].val]
		case "GEOIP":
			r.rules[i].cidrs = ipSets[r.rules[i].val]
		}
	}
	return nil
}

// pick returns the backend name for a target URL. IP-CIDR and GEOIP match a
// literal IP in the URL, or, with dns.enable, any address the host resolves to
// (looked up at most once per call, and only if such a rule is reached).
func (r *router) pick(ctx context.Context, rawURL string) string {
	var host string
	// raw:/raw:// carries inline HTML, not a host.
	if u, err := url.Parse(rawURL); err == nil && !strings.HasPrefix(rawURL, "raw:") {
		host = strings.ToLower(u.Hostname())
	}
	lit, _ := netip.ParseAddr(host)
	var resolved []netip.Addr
	var looked bool
	addrs := func() []netip.Addr {
		if lit.IsValid() {
			return []netip.Addr{lit}
		}
		if r.dns != nil && !looked && host != "" {
			looked = true
			var err error
			if resolved, err = r.dns.lookup(ctx, host); err != nil {
				appconfig.Logger.Warn("dns lookup failed; IP rules skip this host", zap.String("host", host), zap.Error(err))
			}
		}
		return resolved
	}
	for _, ru := range r.rules {
		var ok bool
		switch ru.kind {
		case "DOMAIN":
			ok = host == ru.val
		case "DOMAIN-SUFFIX":
			ok = host == ru.val || strings.HasSuffix(host, "."+ru.val)
		case "DOMAIN-KEYWORD":
			ok = strings.Contains(host, ru.val)
		case "GEOSITE":
			ok = ru.site.match(host)
		case "IP-CIDR", "GEOIP":
			for _, a := range addrs() {
				for _, c := range ru.cidrs {
					if c.Contains(a.Unmap()) {
						ok = true
					}
				}
			}
		case "MATCH":
			ok = true
		}
		if ok {
			return ru.backend
		}
	}
	return "" // unreachable: loadRouter guarantees a trailing MATCH
}

// For returns the backend client for a target URL.
func (r *router) For(ctx context.Context, rawURL string) *c4a.Client {
	return r.clients[r.pick(ctx, rawURL)]
}

// fallback is the MATCH backend, used by calls that carry no URL (ask).
func (r *router) fallback() *c4a.Client { return r.clients[r.rules[len(r.rules)-1].backend] }

// crawl splits URLs by backend and runs the groups in parallel. Pages come back
// grouped by backend (in order of first appearance), not in input order; each
// page carries its URL. A failed backend turns into failed pages, so the others survive.
func (r *router) crawl(ctx context.Context, req c4a.CrawlRequest) (*c4a.CrawlResponse, error) {
	var order []string
	groups := map[string][]string{}
	for _, u := range req.URLs {
		n := r.pick(ctx, u)
		if groups[n] == nil {
			order = append(order, n)
		}
		groups[n] = append(groups[n], u)
	}
	if len(order) == 1 {
		return r.clients[order[0]].Crawl(ctx, req)
	}
	resps := make([]*c4a.CrawlResponse, len(order))
	errs := make([]error, len(order))
	var wg sync.WaitGroup
	for i, n := range order {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub := req
			sub.URLs = groups[n]
			resps[i], errs[i] = r.clients[n].Crawl(ctx, sub)
		}()
	}
	wg.Wait()
	out := &c4a.CrawlResponse{Success: true}
	for i, n := range order {
		if errs[i] != nil {
			out.Success = false
			for _, u := range groups[n] {
				out.Results = append(out.Results, c4a.CrawlResult{URL: u, ErrorMessage: fmt.Sprintf("backend %s: %v", n, errs[i])})
			}
			continue
		}
		out.Success = out.Success && resps[i].Success
		out.Results = append(out.Results, resps[i].Results...)
		out.ServerProcessingTimeS = max(out.ServerProcessingTimeS, resps[i].ServerProcessingTimeS)
	}
	return out, nil
}

// submit sends a job to the one backend all URLs route to. With several
// backends the returned id is "backend:upstream_id", so job() can find its way back.
func (r *router) submit(ctx context.Context, req c4a.CrawlJobRequest) (string, error) {
	if len(req.URLs) == 0 {
		return "", fmt.Errorf("urls must not be empty")
	}
	seen := map[string]bool{}
	for _, u := range req.URLs {
		seen[r.pick(ctx, u)] = true
	}
	if len(seen) != 1 {
		names := make([]string, 0, len(seen))
		for n := range seen {
			names = append(names, n)
		}
		sort.Strings(names)
		return "", fmt.Errorf("urls route to several backends (%s); submit them in separate jobs", strings.Join(names, ", "))
	}
	name := r.pick(ctx, req.URLs[0])
	id, err := r.clients[name].SubmitCrawlJob(ctx, req)
	if err != nil || len(r.clients) == 1 {
		return id, err
	}
	return name + ":" + id, nil
}

func (r *router) job(ctx context.Context, id string) (*c4a.CrawlJobStatus, error) {
	cl, upstreamID := r.fallback(), id
	if len(r.clients) > 1 {
		name, rest, ok := strings.Cut(id, ":")
		cl = r.clients[name]
		if !ok || cl == nil {
			return nil, fmt.Errorf("task_id %q does not name a known backend", id)
		}
		upstreamID = rest
	}
	st, err := cl.CrawlJob(ctx, upstreamID)
	if err != nil {
		return nil, err
	}
	st.TaskID = id
	return st, nil
}
