// Package appconfig centralizes runtime configuration: shared HTTP clients
// (connection reuse), the logger, and the graceful-restart supervisor hook.
package appconfig

import (
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// CrawlHTTPClient is the shared client for upstream Crawl4AI calls.
// No overall timeout: crawls legitimately run for minutes; cancellation is
// carried by the MCP request context. Keep-alives are tuned for many calls
// to a single upstream host.
var CrawlHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 60 * time.Second,
		}).DialContext,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 16, // single-upstream workload; default 2 would thrash
		IdleConnTimeout:     120 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
	},
}

// SearchHTTPClient is the SearXNG client: searches must fail fast.
var SearchHTTPClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
	},
}

// Logger is the process-wide zap logger. It is replaced by Setup so late
// imports still get a working no-op logger during init.
var Logger = zap.NewNop()

// Setup configures the global logger and returns a flush function.
func Setup(production bool) func() {
	var (
		l   *zap.Logger
		err error
	)
	if production {
		l, err = zap.NewProduction()
	} else {
		l, err = zap.NewDevelopment()
	}
	if err != nil {
		panic(err)
	}
	zap.ReplaceGlobals(l)
	Logger = l
	return func() { _ = l.Sync() }
}
