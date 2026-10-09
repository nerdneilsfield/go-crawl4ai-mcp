# go-crawl4ai-mcp

Stateless Streamable HTTP [MCP](https://modelcontextprotocol.io) server that exposes a self-hosted [Crawl4AI](https://github.com/unclecode/crawl4ai) Docker deployment as MCP tools, with an optional [SearXNG](https://docs.searxng.org) web search tool. Built on the official Go MCP SDK and the standard library; no database, no scheduler, no second port.

It exists because Crawl4AI's built-in MCP bridge (`deploy/docker/mcp_bridge.py`) speaks legacy SSE, which several modern MCP clients handle poorly. This server speaks Streamable HTTP instead: stateless, JSON responses, one endpoint.

```
MCP client ── MCP_TOKEN ──> go-crawl4ai-mcp :8080 ── CRAWL4AI_TOKEN ──> Crawl4AI :11235
                                   └── (optional) SearXNG ──> web_search tool
```

## Requirements

- Go 1.25+ to build from source, or Docker.
- A running Crawl4AI server (0.9.x) with its REST API reachable. The upstream container must set `CRAWL4AI_API_TOKEN` if you want authenticated upstream calls.
- Optional: a SearXNG instance with the JSON output format enabled.

## Quick start

1. Build and run:

   ```sh
   go build ./cmd/crawl4ai-mcp
   MCP_TOKEN=letmein CRAWL4AI_URL=http://127.0.0.1:11235 \
     CRAWL4AI_TOKEN=upstream-token ./crawl4ai-mcp
   ```

2. Verify the server is up:

   ```sh
   curl http://127.0.0.1:8080/health
   ```

   `{"status":"ok","version":"…","generation":"0"}` confirms the process is serving. `generation` increments after each graceful restart.

3. Connect an MCP client to `http://127.0.0.1:8080/mcp` with header `Authorization: Bearer letmein`, then call the `md` tool with a URL to confirm the full chain.

With Docker instead:

```sh
docker run -p 8080:8080 -e MCP_TOKEN=letmein \
  -e CRAWL4AI_URL=http://host.docker.internal:11235 \
  -e CRAWL4AI_TOKEN=upstream-token \
  ghcr.io/nerdneilsfield/go-crawl4ai-mcp:latest
```

## Tools

| Tool | Upstream | Enabled | Notes |
|---|---|---|---|
| `md` | `POST /md` | default | URL to Markdown. `f` filter: `fit` (default), `raw`, `bm25`, `llm` (the last two need `q`) |
| `html` | `POST /html` | default | sanitized HTML for schema extraction |
| `crawl` | `POST /crawl` | default | batch crawl (1–100 URLs); returns Markdown + metadata per page; per-page failures are reported individually, not swallowed by the top-level status |
| `screenshot` | `POST /screenshot` | default | returns MCP `ImageContent` (PNG) plus the upstream `artifact_id` |
| `pdf` | `POST /pdf` | default | returns an embedded `application/pdf` resource blob plus `artifact_id` |
| `execute_js` | `POST /execute_js` | flag | needs `--enable-execute-js` here **and** `CRAWL4AI_EXECUTE_JS_ENABLED=true` on the upstream |
| `ask` | `GET /ask` | flag | BM25 search over Crawl4AI's own docs/source; disable with `--enable-ask=false`. Not a general web Q&A tool |
| `submit_crawl` | `POST /crawl/job` | default | async batch crawl; returns `task_id` |
| `get_crawl_job` | `GET /crawl/job/{id}` | default | poll job status (`processing`/`completed`/`failed`) and results |
| `web_search` | SearXNG `GET /search` | URL set | title/URL/snippet only; see [SearXNG](#searxng) |

`browser_config`, `crawler_config`, and `crawler_configs` (per-URL configs with `url_matcher`) are forwarded to `crawl` and `submit_crawl` as raw JSON, so the full CrawlerRunConfig surface stays available without this server re-implementing it.

`web_search` does not crawl its results. The intended loop is: search, pick URLs, then read them with `md` or `crawl`. This avoids launching a browser tab per hit and wasting context on pages the agent did not choose.

`/crawl/stream` (NDJSON) is not exposed; use `submit_crawl` + `get_crawl_job` for long-running batches instead.

## Configuration

Command-line flags take precedence over environment variables.

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `--addr` | `MCP_ADDR` | `:8080` | listen address |
| `--crawl4ai-url` | `CRAWL4AI_URL` | `http://127.0.0.1:11235` | Crawl4AI base URL |
| `--crawl4ai-token` | `CRAWL4AI_TOKEN` | empty | Crawl4AI API token (the upstream's `CRAWL4AI_API_TOKEN`); empty sends no auth |
| `--mcp-token` | `MCP_TOKEN` | empty | Bearer token required from MCP clients; empty disables auth (dev/loopback only) |
| `--crawl-timeout` | `CRAWL_TIMEOUT` | `90s` | per-call upstream timeout. Accepts Go durations (`2m`) or bare seconds (`120`); `0` disables. Applies to Crawl4AI calls only, not SearXNG |
| `--searxng-url` | `SEARXNG_URL` | empty | SearXNG base URL; setting it registers `web_search` |
| `--enable-execute-js` | `ENABLE_EXECUTE_JS` | `false` | expose `execute_js` |
| `--enable-ask` | `ENABLE_ASK` | `true` | expose `ask` |
| `--log-production` | `LOG_PRODUCTION` | `false` | JSON logs (zap production) instead of console format |

Exit status is `0` on clean shutdown and after a graceful restart handoff, `1` on startup or runtime errors.

### SearXNG

Point `SEARXNG_URL` at an instance that has the JSON format enabled in its `settings.yml`; without it the API returns 403 and the tool reports a hint to that effect:

```yaml
use_default_settings: true
search:
  formats:
    - html
    - json
```

`web_search` parameters: `query` (required), `max_results` (default 10, hard cap 20 — truncation of the current result page, not a guarantee), `language`, `categories` (`general`, `news`, `science`, …), `time_range` (`day`/`week`/`month`/`year`), `page`. Results keep SearXNG's ranking and include `engines` and `publishedDate` when present. Engines that time out or get rate-limited are listed in `unresponsive_engines` alongside whatever results succeeded.

SearXNG needs no authentication of its own: keep it on an internal network and let this server's `MCP_TOKEN` be the only public credential.

## Client configuration

Bifrost:

```json
{
  "type": "mcp",
  "connection_type": "http",
  "url": "http://my-host:8080/mcp",
  "headers": { "Authorization": "Bearer letmein" }
}
```

OpenCode:

```json
{
  "mcp": {
    "crawl4ai": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp",
      "headers": { "Authorization": "Bearer letmein" }
    }
  }
}
```

## Behavior notes

- **Protocol versions**: negotiates 2024-11-05 through 2026-07-28. Stateless mode means every POST is self-contained; GET/DELETE on `/mcp` return 405.
- **Cancellation**: with the 2026-07-28 protocol, a client aborting its HTTP request cancels the in-flight upstream crawl immediately. With older protocol versions the MCP-level `notifications/cancelled` cannot reach the stateless handler, so an aborted call may run to completion on the upstream; the `--crawl-timeout` deadline (default 90 s) bounds that.
- **Graceful restart**: sending `SIGUSR1` spawns a replacement process sharing the listening socket, waits for it to report readiness, drains in-flight requests via `http.Server.Shutdown`, then exits 0. The port stays open across the handoff. This serves binary/config reloads without an orchestrator-level rolling restart; it re-execs the current binary, so it cannot pick up a new binary path on disk by itself.
- **Two independent credentials**: MCP clients authenticate to this server with `MCP_TOKEN`; this server authenticates to Crawl4AI with `CRAWL4AI_TOKEN`.

## Operations

- Health check: `GET /health` — no auth, returns status, version, and restart generation.
- Logs: console format by default; `--log-production` for JSON. Key events: startup (addr, upstream, timeout), restart handoff (pids, generation), drain completion.
- Release: `goreleaser release` on a tag builds binaries for linux/darwin/windows (amd64/arm64) and pushes a multi-arch manifest to `ghcr.io/nerdneilsfield/go-crawl4ai-mcp` (tags: `vX.Y.Z`, `latest`). `goreleaser release --snapshot --skip=validate` builds locally without publishing.

## Development

```sh
go build ./... && go vet ./... && go test ./...
```

Layout: `cmd/crawl4ai-mcp` (CLI, auth, wiring), `internal/c4a` (Crawl4AI REST client), `internal/searxng` (SearXNG client), `internal/server` (tool registration and result shaping), `internal/appconfig` (shared HTTP transports, logger), `internal/supervisor` (graceful restart).
