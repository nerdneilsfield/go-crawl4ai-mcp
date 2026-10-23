package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// defaultDNS is used when dns.enable is set without dns.servers.
var defaultDNS = []string{"https://1.1.1.1/dns-query", "https://8.8.8.8/dns-query"}

// resolver turns a host name into addresses over DoH (https://...) or DoT
// (tls://host[:853]). It exists only so IP-CIDR and GEOIP rules can match
// domain URLs; servers are tried in order and the first answer wins.
type resolver struct {
	servers []string
	hc      *http.Client
	tls     *tls.Config // nil = system roots
}

func newResolver(servers []string) (*resolver, error) {
	if len(servers) == 0 {
		servers = defaultDNS
	}
	for _, s := range servers {
		if !strings.HasPrefix(s, "https://") && !strings.HasPrefix(s, "tls://") {
			return nil, fmt.Errorf("dns server %q: want https://... (DoH) or tls://host[:port] (DoT)", s)
		}
	}
	return &resolver{servers: servers, hc: &http.Client{}}, nil
}

func (d *resolver) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	var lastErr error
	for _, s := range d.servers {
		sctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		addrs, err := d.lookupVia(sctx, s, host)
		cancel()
		if err == nil {
			return addrs, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (d *resolver) lookupVia(ctx context.Context, server, host string) ([]netip.Addr, error) {
	var res [2][]netip.Addr
	var errs [2]error
	var wg sync.WaitGroup
	for i, typ := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i], errs[i] = d.query(ctx, server, host, typ)
		}()
	}
	wg.Wait()
	// One family failing is fine as long as the other answered.
	if addrs := append(res[0], res[1]...); len(addrs) > 0 {
		return addrs, nil
	}
	return nil, errors.Join(errs[0], errs[1])
}

func (d *resolver) query(ctx context.Context, server, host string, typ dnsmessage.Type) ([]netip.Addr, error) {
	name, err := dnsmessage.NewName(strings.TrimSuffix(host, ".") + ".")
	if err != nil {
		return nil, err
	}
	// ID 0: RFC 8484 asks for it, and each query has its own connection or request.
	wire, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: name, Type: typ, Class: dnsmessage.ClassINET}},
	}).Pack()
	if err != nil {
		return nil, err
	}
	var reply []byte
	if rest, ok := strings.CutPrefix(server, "tls://"); ok {
		reply, err = d.dot(ctx, rest, wire)
	} else {
		reply, err = d.doh(ctx, server, wire)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", server, err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(reply); err != nil {
		return nil, fmt.Errorf("%s: %w", server, err)
	}
	switch m.RCode {
	case dnsmessage.RCodeSuccess:
	case dnsmessage.RCodeNameError: // NXDOMAIN: an answer, just an empty one
		return nil, nil
	default:
		return nil, fmt.Errorf("%s: rcode %v", server, m.RCode)
	}
	var out []netip.Addr
	for _, a := range m.Answers {
		switch b := a.Body.(type) {
		case *dnsmessage.AResource:
			out = append(out, netip.AddrFrom4(b.A))
		case *dnsmessage.AAAAResource:
			out = append(out, netip.AddrFrom16(b.AAAA))
		}
	}
	return out, nil
}

func (d *resolver) doh(ctx context.Context, url string, wire []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
}

func (d *resolver) dot(ctx context.Context, addr string, wire []byte) ([]byte, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "853")
	}
	conn, err := (&tls.Dialer{Config: d.tls}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	frame := binary.BigEndian.AppendUint16(nil, uint16(len(wire))) // RFC 7858: 2-byte length prefix
	if _, err := conn.Write(append(frame, wire...)); err != nil {
		return nil, err
	}
	var n [2]byte
	if _, err := io.ReadFull(conn, n[:]); err != nil {
		return nil, err
	}
	buf := make([]byte, binary.BigEndian.Uint16(n[:]))
	_, err = io.ReadFull(conn, buf)
	return buf, err
}
