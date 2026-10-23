package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// answer builds a DNS reply: example.test -> 203.0.113.7 (A), nothing for AAAA.
func answer(t *testing.T, wire []byte) []byte {
	t.Helper()
	var q dnsmessage.Message
	if err := q.Unpack(wire); err != nil {
		t.Fatal(err)
	}
	m := dnsmessage.Message{
		Header:    dnsmessage.Header{Response: true, RecursionAvailable: true},
		Questions: q.Questions,
	}
	if q.Questions[0].Type == dnsmessage.TypeA {
		m.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
			Body:   &dnsmessage.AResource{A: [4]byte{203, 0, 113, 7}},
		}}
	}
	out, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestResolverDoH(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/dns-message" {
			http.Error(w, "bad type", 415)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(answer(t, body))
	}))
	defer srv.Close()
	d, err := newResolver([]string{srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	d.hc = srv.Client()
	got, err := d.lookup(context.Background(), "example.test")
	if err != nil || len(got) != 1 || got[0] != netip.MustParseAddr("203.0.113.7") {
		t.Fatalf("lookup = %v, %v", got, err)
	}
}

func TestResolverDoT(t *testing.T) {
	hs := httptest.NewTLSServer(nil) // borrow its self-signed cert
	hs.Close()
	cert := hs.TLS.Certificates[0]
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var n [2]byte
				if _, err := io.ReadFull(c, n[:]); err != nil {
					return
				}
				buf := make([]byte, binary.BigEndian.Uint16(n[:]))
				if _, err := io.ReadFull(c, buf); err != nil {
					return
				}
				a := answer(t, buf)
				c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(a))), a...))
			}()
		}
	}()
	pool := x509.NewCertPool()
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	pool.AddCert(leaf)
	d, err := newResolver([]string{"tls://" + ln.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	d.tls = &tls.Config{RootCAs: pool, ServerName: "example.com"} // httptest's cert is for example.com
	got, err := d.lookup(context.Background(), "example.test")
	if err != nil || len(got) != 1 || got[0] != netip.MustParseAddr("203.0.113.7") {
		t.Fatalf("lookup via :%s = %v, %v", port, got, err)
	}
}

func TestResolverRejectsBadServer(t *testing.T) {
	if _, err := newResolver([]string{"udp://8.8.8.8"}); err == nil {
		t.Error("want error for non-DoH/DoT server")
	}
	d, _ := newResolver(nil)
	if len(d.servers) != 2 {
		t.Errorf("defaults = %v", d.servers)
	}
}

// With dns.enable, an IP-CIDR rule matches a domain URL through its resolved
// address; without it the same rule only sees literal IPs.
func TestRouteUsesDNSOnlyWhenEnabled(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Write(answer(t, body))
	}))
	defer srv.Close()
	dir := t.TempDir()
	yaml := func(dns string) string {
		return "backends: {hk: {url: http://hk}, us: {url: http://us}}\n" + dns +
			"rules: [\"IP-CIDR,203.0.113.0/24,hk\", \"MATCH,us\"]\n"
	}
	for dns, want := range map[string]string{"": "us", "dns: {enable: true, servers: [\"" + srv.URL + "\"]}\n": "hk"} {
		cfg := filepath.Join(dir, "r.yaml")
		os.WriteFile(cfg, []byte(yaml(dns)), 0o600)
		rt, err := loadRouter(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if rt.dns != nil {
			rt.dns.hc = srv.Client()
		}
		if got := rt.pick(context.Background(), "https://example.test/x"); got != want {
			t.Errorf("dns=%q: pick = %s, want %s", dns, got, want)
		}
	}
}
