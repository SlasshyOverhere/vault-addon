# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
# Build
go build -o vault-addon .

# Run (agrees to disclaimer)
./vault-addon --yes

# Run on custom port
PORT=8080 ./vault-addon --yes

# Check version
./vault-addon --version

# Cross-compile (like CI does)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o vault-addon-linux-amd64 .
```

## Project Overview

**vault-addon** is a single-binary Go HTTP server that aggregates publicly available stream links from supported websites. It acts as a Stremio addon (serves `/manifest.json` and `/stream/{type}/{id}`) and provides CDN-redirect extraction via `/extract/`.

There are no tests yet. All source files are `package main`.

## Architecture

### Source Files (all in `package main`)

| File | Responsibility |
|------|---------------|
| `main.go` | Entrypoint, HTTP mux, graceful shutdown, Stremio manifest, disclaimer flow |
| `scraper.go` | Scrapes supported sites: search pages, extract movie/series/pack stream links |
| `resolver.go` | URL redirect-chain resolution: hubcloud → gamerxyt → pixel.hubcloud.cx → Google CDN. Blue button (r2.dev, workers.dev) and pack link resolution. |
| `sites.go` | Site registry — persistent JSON file of source domains (defaults: 4khdhub mirrors). CRUD operations with atomic file writes. |
| `cache.go` | In-memory response cache (`map[string]cacheEntry`) with periodic pruning (15min TTL) and optional disk persistence. |
| `admin.go` | Admin UI (inline HTML/JS) at `/admin` and REST API for site management (`GET/POST /api/sites`, `POST /api/sites/remove`). |
| `updater.go` | Self-update — checks GitHub releases, compares SHA256, replaces binary in-place. |

### Request Flow

1. **Stream request** (`GET /stream/{type}/{id}.json`):
   - Check response cache → return if hit
   - Parse IMDB/TMDB ID → extract season/episode
   - Fetch metadata from Cinemeta (Stremio) or TMDB page
   - Search active source domain for matching title/year
   - Scrape result page HTML with goquery
   - Extract hubcloud/hubdrive links matching episode filter
   - Resolve blue buttons (r2.dev, workers.dev) concurrently via HEAD validation
   - Return JSON stream response (red buttons go through `/extract/`, blue buttons link directly)
   - Cache the response

2. **Extract request** (`GET /extract/?url=...`):
   - Fetch hub page → find gamerxyt link → resolve pixel.hubcloud.cx red button → follow redirect chain to Google CDN URL
   - Returns HTTP 302 redirect to final CDN URL

3. **Admin site management**: Validate that a URL uses the expected HTML template (`.movie-card` elements) before registering.

### Source Site Template

Supported sites must use a specific HTML template:
- Search results return `.movie-card` elements with `.movie-card-title`, `.movie-card-meta`
- Episode pages have `.season-item.episode-item` → `.episode-download-item` → `.episode-file-title`
- Download links are hubcloud/hubdrive URLs extracted from `<a>` tags
- Pack downloads live under `#complete-pack`

### URL Resolution Chain

```
hubcloud URL → hub page HTML → gamerxyt.com/hubcloud.php → pixel.hubcloud.cx/?id=... → redirect chain → Google CDN (video-downloads.googleusercontent.com)
```

Also supports blue button links (FSL/r2.dev and workers.dev) as direct stream URLs without redirect resolution — validated via HEAD request for content-type.

## Key Patterns

- **Concurrent resolution**: Blue button links are validated concurrently via goroutines + WaitGroup (in `extractStreams`, `resolveAndValidateBlue`)
- **Cache layers**: Response cache (response-level, disk-backed, 15min TTL) + CDN cache (in-memory, 30s TTL) for resolved redirect URLs
- **Active domain failover**: Sites are checked for reachability; first responding domain becomes active. Cache invalidated on site add/remove.
- **Atomically persisted state**: Site JSON is written to `.tmp` then renamed (crash-safe)
- **Context-based cancellation**: All external HTTP calls use `context.WithTimeout` (30s for streams, 45s for extracts)

## Dependencies

- `github.com/PuerkitoBio/goquery v1.10.0` — jQuery-like HTML parsing and traversal
- Standard library only otherwise (`net/http`, `encoding/json`, `sync`, `regexp`, `crypto/sha256`, etc.)

## Configuration

- Port: `--port` flag > `PORT` env var > default `51546`
- Disclaimer: `--yes` / `--agree` flag > `AGREE=1` env var > interactive prompt (stored in config dir after first acceptance)
- Config directory: `%APPDATA%\vault-addon` (Windows) or `~/.vault-addon`
- Stored files: `sites.json` (source site registry), `cache.json` (response cache), `agreed` (disclaimer flag)
