# go-crawl4ai-mcp

Stateless Streamable HTTP [MCP](https://modelcontextprotocol.io) server that exposes a self-hosted [Crawl4AI](https://github.com/unclecode/crawl4ai) Docker deployment as MCP tools, with an optional web search tool backed by [DeepSeek Search](https://api-docs.deepseek.com/guides/search/) or [SearXNG](https://docs.searxng.org). Built on the official Go MCP SDK and the standard library; no database, no scheduler, no second port.

It exists because Crawl4AI's built-in MCP bridge (`deploy/docker/mcp_bridge.py`) speaks legacy SSE, which several modern MCP clients handle poorly. This server speaks Streamable HTTP instead: stateless, JSON responses, one endpoint.

```
MCP client ── MCP_TOKEN ──> go-crawl4ai-mcp :8080 ── CRAWL4AI_TOKEN ──> Crawl4AI :11235
                                   └── (optional) DeepSeek Search / SearXNG ──> web_search tool
```

## Requirements

- Go 1.26+ to build from source, or Docker.
- A running Crawl4AI server (0.9.x) with its REST API reachable. The upstream container must set `CRAWL4AI_API_TOKEN` if you want authenticated upstream calls.
- Optional, for `web_search`: a DeepSeek API key, or a SearXNG instance with the JSON output format enabled.

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

### Routing across several Crawl4AI backends

If you run Crawl4AI in more than one region, give the server a routing file instead of a single `CRAWL4AI_URL`. Start from the examples in this repository:

- [`examples/routes.yaml`](examples/routes.yaml): backends, rules, and the optional `geo` and `dns` sections, commented.
- [`examples/docker-compose.routing.yml`](examples/docker-compose.routing.yml): the MCP server with that file mounted and a volume for the geo databases.

```sh
cp examples/routes.yaml routes.yaml     # edit backends and rules
crawl4ai-mcp --route-config routes.yaml # or: ROUTE_CONFIG=routes.yaml
```

With Docker, mount the file and pass its path:

```sh
docker run -p 8080:8080 -e MCP_TOKEN=letmein \
  -e ROUTE_CONFIG=/routes.yaml -v "$PWD/routes.yaml:/routes.yaml:ro" \
  ghcr.io/nerdneilsfield/go-crawl4ai-mcp:latest
```

The startup log tells you it worked: a `downloading geo database` line the first time a `GEOSITE` or `GEOIP` rule is used, then `listening`. A config error (unknown backend, missing `MATCH`, unknown category) stops the server with the reason. Rule syntax, the databases and DNS are covered in [the reference below](#routing-to-several-crawl4ai-backends).

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
| `web_search` | DeepSeek `POST /v1/tools/search` or SearXNG `GET /search` | key or URL set | title/URL/snippet (DeepSeek adds summary and rerank score); see [Search providers](#search-providers) |

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
| `--route-config` | `ROUTE_CONFIG` | empty | YAML file that routes URLs to several Crawl4AI backends ([see below](#routing-to-several-crawl4ai-backends)); when set, `--crawl4ai-url`/`--crawl4ai-token` are ignored |
| `--mcp-token` | `MCP_TOKEN` | empty | Bearer token required from MCP clients; empty disables auth (dev/loopback only) |
| `--crawl-timeout` | `CRAWL_TIMEOUT` | `90s` | per-call upstream timeout. Accepts Go durations (`2m`) or bare seconds (`120`); `0` disables. Applies to Crawl4AI calls only, not web search |
| `--searxng-url` | `SEARXNG_URL` | empty | SearXNG base URL; enables the SearXNG backend of `web_search` |
| `--deepseek-api-key` | `DEEPSEEK_API_KEY` | empty | DeepSeek Search API key; enables the DeepSeek backend of `web_search` |
| `--deepseek-url` | `DEEPSEEK_URL` | `https://api.deepseek.com` | DeepSeek API base URL; override for proxies or OpenAI-compatible gateways |
| `--search-provider` | `SEARCH_PROVIDER` | `auto` | `web_search` backend: `auto`, `deepseek` or `searxng` ([see below](#search-providers)) |
| `--enable-execute-js` | `ENABLE_EXECUTE_JS` | `false` | expose `execute_js` |
| `--enable-ask` | `ENABLE_ASK` | `true` | expose `ask` |
| `--log-production` | `LOG_PRODUCTION` | `false` | JSON logs (zap production) instead of console format |

### Routing to several Crawl4AI backends

Point `--route-config` at a YAML file to send each URL to a different backend (say, one per region). Rules are checked top to bottom, first match wins, and the last rule must be `MATCH`. Without `--route-config` everything goes to the single `--crawl4ai-url`.

```yaml
backends:
  hk: {url: http://hk.example:11235, token: xxx}
  us: {url: http://us.example:11235, token: yyy}
rules:
  - DOMAIN-SUFFIX,bilibili.com,hk   # the domain itself and its subdomains
  - DOMAIN,example.org,us           # exact host
  - DOMAIN-KEYWORD,google,us
  - GEOSITE,google,us               # category from geosite.dat
  - GEOSITE,cn,hk
  - GEOIP,cn,hk                     # country from geoip.dat
  - IP-CIDR,10.0.0.0/8,hk
  - MATCH,us
```

| Rule | Matches |
|---|---|
| `DOMAIN,host,backend` | exactly that host |
| `DOMAIN-SUFFIX,example.com,backend` | `example.com` and every subdomain |
| `DOMAIN-KEYWORD,word,backend` | hosts containing `word` |
| `GEOSITE,category,backend` | hosts in a [geosite](#geosite-and-geoip-databases) category (`cn`, `google`, ...) |
| `IP-CIDR,a.b.c.d/n,backend` | a literal IP in the URL, or a resolved address when [DNS](#dns-lookups-for-ip-rules) is on |
| `GEOIP,cn,backend` | same, against a country in geoip |
| `MATCH,backend` | everything; must be last |

How tools use it:

- `crawl` splits its URLs by backend and runs the groups in parallel. Pages come back grouped by backend, not in input order. If one backend fails, its URLs come back as failed pages and the rest are kept.
- `submit_crawl` needs all URLs to land on one backend, otherwise it errors. With several backends its `task_id` is `backend:id`, and `get_crawl_job` uses the prefix to find the right backend. Backend names therefore cannot contain `:`.
- `ask` and `raw:` URLs go to the `MATCH` backend.
- Rules look at the URL you pass in, not at where redirects lead.

#### geosite and geoip databases

`GEOSITE` and `GEOIP` read the v2ray-format `geosite.dat` and `geoip.dat` that mihomo, Xray and sing-box all use. You do not need to download them: when a rule needs one and it is missing, the server fetches it at startup from [meta-rules-dat](https://github.com/MetaCubeX/meta-rules-dat). To use a different source or location:

```yaml
geo:
  dir: /data/geo                    # default: <user cache dir>/crawl4ai-mcp
  geosite_url: https://example.com/geosite.dat
  geoip_url: https://example.com/geoip.dat
  update_interval: 24h              # default 24h; 0s turns refreshing off
```

- Only the categories your rules name are loaded; an unknown category is a startup error. Category names are case-insensitive. The `@attr` filters (`cn@ads`) are not supported, and neither are `.mmdb` or `.mrs` files.
- **Refreshing**: every `update_interval` (default 24h) the server downloads both files again in the background. Each one is parsed before it replaces anything, so an error page, a truncated file or a missing category is rejected and the data in use stays; the new data takes effect without a restart. Failures are logged as `geo database refresh failed` and retried at the next interval.
- **On start**, a file older than `update_interval` is refreshed first. If that download fails, the old file is used, so a restart while offline still works. Only a missing file with no way to download it stops the server.
- With `update_interval: 0s` nothing is ever re-downloaded after the first fetch; replace the files yourself and restart.
- In Docker, with no `geo.dir` the files go to `/home/nonroot/.cache/crawl4ai-mcp` inside the container. That works, but a new container downloads them again. To keep them, mount a volume, point `geo.dir` at it, and make sure uid 65532 (the image's `nonroot` user) can write there. A fresh named volume belongs to root and fails with `mkdir ...: permission denied`; [`examples/docker-compose.routing.yml`](examples/docker-compose.routing.yml) shows a one-shot `chown` that fixes it. With a bind mount, `chown 65532:65532` the host directory instead.
- If the server cannot reach GitHub (for example behind a firewall), point `geosite_url` / `geoip_url` at a mirror. Or place the two files in `geo.dir` yourself and set `update_interval: 0s` so the server never tries to download.

#### DNS lookups for IP rules

By default `IP-CIDR` and `GEOIP` only match when the URL itself contains an IP, so `GEOIP,cn` does not catch `https://www.baidu.com`. Turn on DNS to make them resolve the host first:

```yaml
dns:
  enable: true                      # default false
  servers:                          # default: the two DoH servers below
    - https://1.1.1.1/dns-query     # DoH
    - https://8.8.8.8/dns-query
    - tls://dns.google              # DoT, port 853 unless given
```

- Servers are tried in order; the first that answers wins. A host that cannot be resolved just skips IP rules and falls through to later ones.
- Only DoH (`https://`) and DoT (`tls://`) are supported, with certificate checks. There is no plain UDP DNS and no cache; a lookup happens only when a request reaches an `IP-CIDR` or `GEOIP` rule.
- The answer depends on where the resolver sees you from. Sites with geo-aware DNS return overseas addresses to an overseas resolver, so `www.baidu.com` may resolve outside CN even though it is a Chinese site. `GEOSITE` classifies by name and does not have this problem; prefer it for domains and keep `GEOIP` as a fallback.

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

### Search providers

`web_search` has two backends, picked by `SEARCH_PROVIDER`:

- `auto` (default): DeepSeek when `DEEPSEEK_API_KEY` is set, otherwise SearXNG when `SEARXNG_URL` is set. With neither, `web_search` is not registered.
- `deepseek`: always DeepSeek. Startup fails if the key is missing.
- `searxng`: always SearXNG, even when a DeepSeek key is set. Startup fails if `SEARXNG_URL` is missing.

SearXNG stays the free, self-hosted option. [DeepSeek Search](https://api-docs.deepseek.com/guides/search/) uses DeepSeek's own index with reranking and is billed at $0.75 per 1,000 successful requests; empty results are free. Insufficient balance (HTTP 402) is reported as such in the tool error. The API allows 100 QPS. There is no retry or cache.

With DeepSeek, each result carries `snippet`, a longer `summary`, `publishedDate`, `rerankScore` (0–3; 1.5 and above is high quality) and `authority` (0–4, −1 unknown). The tool never asks DeepSeek for full page content; read pages with `md` or `crawl`. `max_results` maps to DeepSeek's `count`.

DeepSeek has no language, time range, category or page parameters, so:

- `language` becomes a `lang:` operator on the primary language code (`zh-CN` → `lang:zh`).
- `time_range` becomes `after:YYYY-MM-DD` relative to the server's current date.
- `categories` is ignored.
- `page` above 1 is ignored and a `notice` field says so.

`query` is passed through unchanged, so DeepSeek operators work: `site:`, `-site:`, `-word`, `-"phrase"`, `"phrase"`, `lang:zh,en`, `after:2025`, `before:2026`. Queries are limited to 128 characters.

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

Layout: `cmd/crawl4ai-mcp` (CLI, auth, wiring), `internal/c4a` (Crawl4AI REST client), `internal/searxng` (SearXNG client), `internal/deepseek` (DeepSeek Search client), `internal/server` (tool registration and result shaping), `internal/appconfig` (shared HTTP transports, logger).
