# go-crawl4ai-mcp

A lightweight, native **Streamable HTTP MCP server** for a self-hosted
[Crawl4AI](https://github.com/unclecode/crawl4ai) Docker deployment. Written in
Go with only the official MCP Go SDK and the standard library — no Fiber, no
database, no internal scheduler.

It replaces Crawl4AI's built-in legacy SSE MCP bridge (`deploy/docker/mcp_bridge.py`)
with a stateless Streamable HTTP endpoint that modern MCP clients (Bifrost,
OpenCode, Hermes) can connect to directly.

```
MCP client ── Bearer token ──> go-crawl4ai-mcp (/mcp) ── CRAWL4AI_API_TOKEN ──> Crawl4AI Docker (:11235)
```

## Tools

Mirrors the official Crawl4AI MCP tool names and semantics:

| Tool | Upstream endpoint | Notes |
|---|---|---|
| `md` | `POST /md` | filters: `fit` (default), `raw`, `bm25`, `llm` |
| `html` | `POST /html` | sanitized HTML for schema extraction |
| `crawl` | `POST /crawl` | batch crawl; returns markdown + metadata per page, **preserves per-page failures** |
| `screenshot` | `POST /screenshot` | returns MCP `ImageContent` (PNG) + artifact pointer |
| `pdf` | `POST /pdf` | returns embedded `application/pdf` resource blob + artifact pointer |
| `execute_js` | `POST /execute_js` | opt-in via `ENABLE_EXECUTE_JS=true` (upstream must also set `CRAWL4AI_EXECUTE_JS_ENABLED=true`) |
| `ask` | `GET /ask` | Crawl4AI's own docs/code context (BM25); opt-out via `ENABLE_ASK=false` |
| `submit_crawl` | `POST /crawl/job` | async job submission, returns `task_id` |
| `get_crawl_job` | `GET /crawl/job/{id}` | poll job status/results |

Notes on result shapes (verified against `deploy/docker/server.py` 0.9.x):

- `/crawl` may return top-level `"success": true` while individual pages fail;
  the `crawl` tool reports `failed_count` and per-page `error` accurately.
- Screenshots/PDFs come back as base64 + `artifact_id` (the legacy
  `output_path` was removed); they are decoded into proper MCP content types
  instead of bloating text content.
- `browser_config` / `crawler_config` / `crawler_configs` are forwarded as raw
  JSON objects so the full CrawlerRunConfig surface stays available without
  re-implementing it in Go.
- `/crawl/stream` (NDJSON) is intentionally not exposed; use
  `submit_crawl` + `get_crawl_job` for long-running batches.

## Configuration

Flags or environment variables:

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-addr` | `MCP_ADDR` | `:8080` | listen address |
| `-crawl4ai-url` | `CRAWL4AI_URL` | `http://127.0.0.1:11235` | Crawl4AI base URL |
| `-crawl4ai-token` | `CRAWL4AI_TOKEN` | — | Crawl4AI API token (`CRAWL4AI_API_TOKEN` on the upstream) |
| `-mcp-token` | `MCP_TOKEN` | — | Bearer token for MCP clients; empty disables auth (dev only) |
| | `ENABLE_EXECUTE_JS` | `false` | expose `execute_js` |
| | `ENABLE_ASK` | `true` | expose `ask` |

## Run

```sh
go build ./cmd/crawl4ai-mcp
MCP_TOKEN=secret CRAWL4AI_URL=http://127.0.0.1:11235 CRAWL4AI_TOKEN=upstream-token ./crawl4ai-mcp
```

Endpoints: `POST /mcp` (MCP Streamable HTTP, stateless, JSON responses),
`GET /health`.

### Bifrost

```json
{
  "type": "mcp",
  "connection_type": "http",
  "url": "http://my-host:8080/mcp",
  "headers": { "Authorization": "Bearer secret" }
}
```

### OpenCode

```json
{
  "mcp": {
    "crawl4ai": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp",
      "headers": { "Authorization": "Bearer secret" }
    }
  }
}
```

## Design

- **Stateless Streamable HTTP** (`Stateless: true`): every POST is
  self-contained; GET/DELETE on `/mcp` return 405. Works for the 2026-07-28
  protocol and negotiates down to older versions (2024-11-05+ verified).
- **`JSONResponse: true`**: tool calls return `application/json` instead of a
  standing SSE stream — simpler for proxies and clients, still spec-compliant.
- **`PropagateRequestCancellation: true`**: a client abort cancels the in-flight
  upstream crawl (effective on the 2026-07-28 protocol where the POST is the
  whole request lifecycle).
- Two independent credentials: MCP clients authenticate to this server
  (`MCP_TOKEN`); this server authenticates to Crawl4AI (`CRAWL4AI_TOKEN`).

## Development

```sh
go build ./... && go vet ./... && go test ./...
```
