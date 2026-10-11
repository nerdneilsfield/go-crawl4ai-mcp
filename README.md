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

   `{"status":"ok","version":"…"}` confirms the process is serving.

3. Connect an MCP client to `http://127.0.0.1:8080/mcp` with header `Authorization: Bearer letmein`, then call the `md` tool with a URL to confirm the full chain.

With Docker instead:

```sh
docker run -p 8080:8080 -e MCP_TOKEN=letmein \
  -e CRAWL4AI_URL=http://host.docker.internal:11235 \
  -e CRAWL4AI_TOKEN=upstream-token \
  ghcr.io/nerdneilsfield/go-crawl4ai-mcp:latest
```

### Full stack with Docker Compose

The compose file below brings up the three components together: Crawl4AI (the crawl engine), SearXNG (meta-search, JSON format pre-enabled), and this MCP server. Save it as `docker-compose.yml` and run `docker compose up -d`:

```yaml
services:
  crawl4ai:
    image: unclecode/crawl4ai:latest
    restart: unless-stopped
    shm_size: 1g # the headless browser needs it; smaller values crash on heavy pages
    ports:
      - "11235:11235"
    environment:
      CRAWL4AI_API_TOKEN: upstream-token # CRAWL4AI_TOKEN on the MCP server; 0.9.x binds loopback without one

  searxng:
    image: searxng/searxng:latest
    restart: unless-stopped
    volumes:
      - ./searxng:/etc/searxng # must contain settings.yml with JSON format (below)

  crawl4ai-mcp:
    image: ghcr.io/nerdneilsfield/go-crawl4ai-mcp:latest
    restart: unless-stopped
    depends_on:
      - crawl4ai
      - searxng
    ports:
      - "8080:8080"
    environment:
      MCP_TOKEN: letmein # what your MCP client sends
      CRAWL4AI_URL: http://crawl4ai:11235
      CRAWL4AI_TOKEN: upstream-token
      SEARXNG_URL: http://searxng:8080
```

SearXNG refuses JSON responses unless the format is enabled. Create `./searxng/settings.yml` before starting:

```yaml
use_default_settings: true
search:
  formats:
    - html
    - json
```

MCP clients connect to `http://<host>:8080/mcp` with `Authorization: Bearer letmein`. All three containers talk on the internal compose network; only :8080 needs to be reachable from the client (and :11235 if you want direct browser access to the Crawl4AI playground).

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
| `current_time` | — (local) | default | current date/time in a chosen IANA time zone (default UTC): RFC3339, ISO week, weekday; useful for grounding "now" |
| `sitemap_urls` | direct fetch | default | list URLs from a sitemap.xml, sitemapindex, plain-text list or robots.txt; discovery before reading |
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

Exit status is `0` on clean shutdown (SIGTERM/SIGINT drain in-flight requests, up to 25 s) and `1` on startup or runtime errors.

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

All examples below connect to the Streamable HTTP endpoint at `http://<host>:8080/mcp` with `Authorization: Bearer $MCP_TOKEN`. Pick the section for your client; adjust the host and token.

### Bifrost (`config.json`)

```json
{
  "mcp": {
    "client_configs": [
      {
        "name": "crawl4ai",
        "connection_type": "http",
        "connection_string": "http://my-host:8080/mcp",
        "auth_type": "headers",
        "headers": { "Authorization": "env.CRAWL4AI_MCP_AUTH" },
        "tools_to_execute": ["*"]
      }
    ]
  }
}
```

The `Authorization` value is resolved from the `CRAWL4AI_MCP_AUTH` environment variable on the Bifrost host; it must contain the full `Bearer letmein` string.

### Codex CLI (`~/.codex/config.toml`)

```toml
[mcp_servers.crawl4ai]
url = "http://127.0.0.1:8080/mcp"
http_headers = { "Authorization" = "Bearer letmein" }
```

Or as JSON args in `~/.codex/mcp.json`-style entries for launchers that use `command` form:

```json
{
  "mcpServers": {
    "crawl4ai": {
      "command": "npx",
      "args": ["-y", "mcp-remote", "http://127.0.0.1:8080/mcp", "--header", "Authorization: Bearer letmein"]
    }
  }
}
```

Use the `mcp-remote` bridge when the client only speaks stdio.

### Claude Code

```sh
claude mcp add --transport http crawl4ai http://127.0.0.1:8080/mcp \
  --header "Authorization: Bearer letmein"
```

Or in `.mcp.json` at the project root:

```json
{
  "mcpServers": {
    "crawl4ai": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp",
      "headers": { "Authorization": "Bearer letmein" }
    }
  }
}
```

### OpenCode

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

### Claude Desktop

Claude Desktop speaks stdio only; bridge with `mcp-remote` in `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "crawl4ai": {
      "command": "npx",
      "args": ["-y", "mcp-remote", "http://127.0.0.1:8080/mcp", "--header", "Authorization: Bearer letmein"]
    }
  }
}
```

### Cursor

`~/.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "crawl4ai": {
      "url": "http://127.0.0.1:8080/mcp",
      "headers": { "Authorization": "Bearer letmein" }
    }
  }
}
```

Or add via the Cursor UI: **Settings → MCP → Add new global MCP server**, type `HTTP`, URL `http://127.0.0.1:8080/mcp`, header `Authorization: Bearer letmein`.

## Behavior notes

- **Protocol versions**: negotiates 2024-11-05 through 2026-07-28. Stateless mode means every POST is self-contained; GET/DELETE on `/mcp` return 405.
- **Cancellation**: with the 2026-07-28 protocol, a client aborting its HTTP request cancels the in-flight upstream crawl immediately. With older protocol versions the MCP-level `notifications/cancelled` cannot reach the stateless handler, so an aborted call may run to completion on the upstream; the `--crawl-timeout` deadline (default 90 s) bounds that.
- **Unauthenticated loopback mode**: with no `MCP_TOKEN`, the server refuses to bind a non-loopback address and exits 1. Bind `127.0.0.1` for local-only use, or set `MCP_TOKEN` to listen on all interfaces.
- **Two independent credentials**: MCP clients authenticate to this server with `MCP_TOKEN`; this server authenticates to Crawl4AI with `CRAWL4AI_TOKEN`.

## Operations

- Health check: `GET /health` — no auth, returns status and version.
- Logs: console format by default; `--log-production` for JSON. Key events: startup (addr, upstream, timeout) and drain completion on shutdown.
- Release: `goreleaser release` on a tag builds binaries for linux/darwin/windows (amd64/arm64) and pushes a multi-arch manifest to `ghcr.io/nerdneilsfield/go-crawl4ai-mcp` (tags: `vX.Y.Z`, `latest`). `goreleaser release --snapshot --skip=validate` builds locally without publishing.

## Development

```sh
go build ./... && go vet ./... && go test ./...
```

Layout: `cmd/crawl4ai-mcp` (CLI, auth, wiring), `internal/c4a` (Crawl4AI REST client), `internal/searxng` (SearXNG client), `internal/server` (tool registration and result shaping), `internal/appconfig` (shared HTTP transports, logger).
