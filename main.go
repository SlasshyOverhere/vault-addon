package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	defaultPort    = "51546"
	currentVersion = "0.1.1"
)

var port string

func main() {
	port = os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	showDisclaimer()
	if err := initSiteRegistry(); err != nil {
		log.Fatalf("Failed to load site registry: %v", err)
	}
	go checkForUpdates()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /manifest.json", handleManifest)
	mux.HandleFunc("GET /stream/{type}/{rest...}", handleStream)
	mux.HandleFunc("GET /extract/", handleExtract)
	mux.HandleFunc("GET /admin", handleAdmin)
	mux.HandleFunc("GET /api/sites", handleAPIListSites)
	mux.HandleFunc("POST /api/sites", handleAPIAddSite)
	mux.HandleFunc("POST /api/sites/remove", handleAPIRemoveSite)
	mux.HandleFunc("GET /{$}", handleDocs)
	mux.HandleFunc("GET /", handleDocs)

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		fmt.Printf("Addon running on http://localhost:%s\n", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	fmt.Println("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)
}

func configDir() string {
	if runtime.GOOS == "windows" {
		appData := os.Getenv("APPDATA")
		if appData != "" {
			return filepath.Join(appData, "vault-addon")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".vault-addon")
}

func disclaimerAgreed() bool {
	dir := configDir()
	data, err := os.ReadFile(filepath.Join(dir, "agreed"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "1"
}

func saveDisclaimerAgreement() {
	dir := configDir()
	os.MkdirAll(dir, 0755)
	os.WriteFile(filepath.Join(dir, "agreed"), []byte("1"), 0644)
}

func showDisclaimer() {
	for _, arg := range os.Args[1:] {
		if arg == "--yes" || arg == "--agree" {
			saveDisclaimerAgreement()
			return
		}
	}
	if os.Getenv("AGREE") == "1" {
		saveDisclaimerAgreement()
		return
	}
	if disclaimerAgreed() {
		return
	}

	fmt.Println(`
╔══════════════════════════════════════════════════════════════╗
║                      DISCLAIMER                              ║
╠══════════════════════════════════════════════════════════════╣
║  This software is for EDUCATIONAL and PERSONAL USE ONLY.    ║
║                                                              ║
║  - Scrapes publicly available URLs from public websites.     ║
║  - Does not host, store, or distribute any media content.    ║
║  - Not affiliated with any content provider or service.      ║
║  - Users are solely responsible for compliance with local    ║
║    laws. The developers assume no liability for misuse.      ║
║  - Provided without any warranty, express or implied.        ║
║                                                              ║
║  By continuing, you agree to these terms.                    ║
╚══════════════════════════════════════════════════════════════╝`)

	fmt.Print("Do you agree? (y/n): ")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	if answer != "y" && answer != "yes" {
		fmt.Println("Exiting.")
		os.Exit(0)
	}
	saveDisclaimerAgreement()
}

// --- Stremio manifest ---

type Manifest struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Resources   []string `json:"resources"`
	Types       []string `json:"types"`
	IDPrefixes  []string `json:"idPrefixes"`
	Catalogs    []any    `json:"catalogs"`
}

var manifest = Manifest{
	ID:          "org.vault-addon",
	Version:     "0.2.0",
	Name:        "vault-addon",
	Description: "HTTP streams with CDN redirect. No debrid needed.",
	Resources:   []string{"stream"},
	Types:       []string{"movie", "series"},
	IDPrefixes:  []string{"tt", "tmdb:"},
	Catalogs:    []any{},
}

func handleManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(manifest)
}

// --- Stream handler ---

type Stream struct {
	URL            string         `json:"url"`
	Name           string         `json:"name"`
	Title          string         `json:"title"`
	BehaviorHints  map[string]any `json:"behaviorHints,omitempty"`
}

type StreamResponse struct {
	Streams []Stream `json:"streams"`
}

func handleStream(w http.ResponseWriter, r *http.Request) {
	itype := r.PathValue("type")
	rawID := r.PathValue("rest")

	// Remove .json suffix if present
	rawID = strings.TrimSuffix(rawID, ".json")

	if !strings.HasPrefix(rawID, "tt") && !strings.HasPrefix(rawID, "tmdb:") {
		writeJSON(w, StreamResponse{Streams: []Stream{}})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Parse season/episode/full from ID
	parts := strings.Split(rawID, ":")
	var imdbOrTmdb string
	season := 0
	episode := 0
	isFullSeason := false
	isSeries := itype == "series"

	if strings.HasPrefix(rawID, "tmdb:") {
		// tmdb:1399:1:1 or tmdb:1399:1:full
		imdbOrTmdb = rawID
		if len(parts) >= 3 {
			fmt.Sscanf(parts[1], "%d", &season)
			if parts[2] == "full" {
				isFullSeason = true
			} else {
				fmt.Sscanf(parts[2], "%d", &episode)
			}
		}
	} else {
		// tt0944947:6:4 or tt0944947:6:full or tt0111161
		imdbOrTmdb = parts[0]
		if len(parts) >= 2 {
			fmt.Sscanf(parts[1], "%d", &season)
			isSeries = true
		}
		if len(parts) >= 3 {
			if parts[2] == "full" {
				isFullSeason = true
			} else {
				fmt.Sscanf(parts[2], "%d", &episode)
			}
		}
	}

	// Get meta
	meta, err := getMeta(ctx, imdbOrTmdb, itype)
	if err != nil || meta == nil || meta.Name == "" {
		writeJSON(w, StreamResponse{Streams: []Stream{}})
		return
	}

	// Find page
	pageURL, err := findPageURL(ctx, meta.Name, meta.Year, isSeries)
	if err != nil || pageURL == "" {
		writeJSON(w, StreamResponse{Streams: []Stream{}})
		return
	}

	// Fetch page
	html, err := fetchPageHTML(ctx, pageURL)
	if err != nil || html == "" {
		writeJSON(w, StreamResponse{Streams: []Stream{}})
		return
	}

	// Build base URL for /extract/ links
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	baseURL := fmt.Sprintf("%s://%s", scheme, r.Host)

	var streams []Stream

	if isSeries && isFullSeason {
		streams, _ = extractPackStreams(ctx, html, season, meta.Name, baseURL)
	} else if isSeries {
		sp := fmt.Sprintf("%02d", season)
		ep := fmt.Sprintf("%02d", episode)
		streams = extractStreams(ctx, html, func(epNum string) bool {
			return strings.Contains(epNum, "S"+sp)
		}, meta.Name, baseURL)
		// Filter to specific episode
		filtered := make([]Stream, 0, len(streams))
		for _, s := range streams {
			titleFirstLine := s.Title
			if idx := strings.Index(titleFirstLine, "\n"); idx >= 0 {
				titleFirstLine = titleFirstLine[:idx]
			}
			if strings.Contains(titleFirstLine, "Episode-"+ep) || strings.Contains(titleFirstLine, "S"+sp+"E"+ep) {
				filtered = append(filtered, s)
			}
		}
		streams = filtered
	} else {
		streams = extractMovieStreams(ctx, html, meta.Name, baseURL)
	}

	if streams == nil {
		streams = []Stream{}
	}
	writeJSON(w, StreamResponse{Streams: streams})
}

// --- Extract handler ---

func handleExtract(w http.ResponseWriter, r *http.Request) {
	hubURL := r.URL.Query().Get("url")
	if hubURL == "" {
		http.Error(w, `{"error":"missing url"}`, http.StatusBadRequest)
		return
	}

	log.Printf("[/extract] %.80s...", hubURL)

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	redURL, err := resolveRedButtonFromHub(ctx, hubURL)
	if err != nil || redURL == "" {
		http.Error(w, `{"error":"could not resolve video URL"}`, http.StatusBadGateway)
		return
	}

	googleURL, err := resolveRedButton(ctx, redURL)
	if err != nil || googleURL == "" {
		log.Printf("  -> red button resolution failed")
		http.Error(w, `{"error":"could not resolve video URL"}`, http.StatusBadGateway)
		return
	}

	log.Printf("  -> redirecting to google CDN")
	http.Redirect(w, r, googleURL, http.StatusFound)
}

// --- Docs page ---

func handleDocs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>vault-addon</title>
<style>body{font-family:system-ui;max-width:800px;margin:2rem auto;padding:0 1rem;line-height:1.6;color:#eee;background:#111}
h1{color:#0cf}h2{color:#fa0;border-bottom:1px solid #333;padding-bottom:.3rem}code{background:#222;padding:.15rem .4rem;border-radius:3px;font-size:.9em}
table{border-collapse:collapse;width:100%%}th,td{border:1px solid #333;padding:.4rem .6rem;text-align:left}th{background:#1a1a1a}
.endpoint{background:#1a2a1a;border-left:3px solid #0f0;padding:.5rem 1rem;margin:.5rem 0}</style></head><body>
<h1>vault-addon</h1>
<p>Open-source link aggregator for movie &amp; series streams. Scrapes publicly available pages and provides direct CDN download links.</p>
<p><a href="https://github.com/SlasshyOverhere/vault-addon" style="color:#0cf">GitHub</a> &middot; <a href="https://github.com/SlasshyOverhere/vault-addon/blob/main/LICENSE" style="color:#0cf">MIT License</a> &middot; <a href="/admin" style="color:#0cf">Admin Panel</a> &middot; v0.1.1</p>

<h2>About</h2>
<p>vault-addon is a lightweight, single-binary HTTP server that aggregates publicly available stream links from public websites. Written in Go for fast startup and minimal resource usage. No external dependencies required at runtime.</p>
<ul>
<li>No media hosting — links point to third-party servers</li>
<li>No API keys or accounts required</li>
<li>Supports movies, series episodes, and full season packs</li>
<li>Resolves redirect chains to direct CDN download URLs</li>
</ul>

<h2>Endpoints</h2>

<div class="endpoint"><strong>GET /manifest.json</strong><br>Addon manifest</div>

<div class="endpoint"><strong>GET /stream/{type}/{id}.json</strong><br>Get streams for a movie or series episode.<br>
<table><tr><th>Parameter</th><th>Description</th><th>Example</th></tr>
<tr><td>type</td><td><code>movie</code> or <code>series</code></td><td><code>series</code></td></tr>
<tr><td>id</td><td>IMDB ID with season/episode</td><td><code>tt0944947:6:4</code></td></tr>
</table>
<p><strong>Full season packs:</strong> Use <code>full</code> instead of episode number to get zip/download links for an entire season.</p>
<p>Example: <code>/stream/series/tt11198330:1:full.json</code> → all S01 pack download links</p>
<p>Regular episode: <code>/stream/series/tt0944947:6:4.json</code> → S06E04 streams</p>
</div>

<div class="endpoint"><strong>GET /extract/?url=ENCODED_URL</strong><br>Resolves a source URL through the redirect chain → 302 redirects to final CDN URL.<br>
<p>Used internally for playable streams. Pack streams (zip files) are resolved to direct download links instead.</p>
</div>

<h2>ID Formats</h2>
<table><tr><th>Format</th><th>Description</th><th>Example</th></tr>
<tr><td><code>tt{imdb}:{season}:{episode}</code></td><td>Series episode</td><td><code>tt0944947:6:4</code></td></tr>
<tr><td><code>tt{imdb}:{season}:full</code></td><td>Full season pack</td><td><code>tt11198330:1:full</code></td></tr>
<tr><td><code>tt{imdb}</code></td><td>Movie</td><td><code>tt0111161</code></td></tr>
<tr><td><code>tmdb:{id}:{season}:{episode}</code></td><td>Series by TMDB ID</td><td><code>tmdb:1399:1:1</code></td></tr>
</table>

<h2>Notes</h2>
<ul>
<li>Pack streams (zip files) are download-only — not directly playable.</li>
<li>Server uses port <code>%s</code> by default. Override with <code>PORT=XXXX</code> env var.</li>
</ul>
</body></html>`, port)
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
