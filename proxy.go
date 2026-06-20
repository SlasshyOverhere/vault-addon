// proxy.go — Streaming HTTP proxy with 206 Partial Content support.
//
// NOTE: This proxy is currently UNUSED for Google CDN URLs because
// Google CDN (video-downloads.googleusercontent.com) ignores Range
// headers and always returns 200 with the full file. Proxying adds
// skip-through delay with no benefit — /extract/ redirects directly
// to Google CDN instead.
//
// The proxy is kept for future use if a CDN that supports Range
// requests is discovered (e.g., workers.dev/r2.dev links already
// support native 206 and don't need proxying either).

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var proxyClient = &http.Client{
	Timeout: 0,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
	Transport: &http.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     5 * time.Minute,
		WriteBufferSize:     1024 * 1024,
		ReadBufferSize:      1024 * 1024,
	},
}

func handleProxy(w http.ResponseWriter, r *http.Request) {
	targetURL := r.URL.Query().Get("url")
	if targetURL == "" {
		http.Error(w, `{"error":"missing url"}`, http.StatusBadRequest)
		return
	}
	proxyOrExtract(targetURL, w, r)
}

// proxyOrExtract streams content from a CDN URL with 206 support.
// No buffering, no pre-download — direct pipe from upstream to client.
// When client disconnects, the pipe breaks and upstream connection closes immediately.
func proxyOrExtract(targetURL string, w http.ResponseWriter, r *http.Request) {
	rangeHeader := r.Header.Get("Range")

	// No Range → redirect to CDN directly
	if rangeHeader == "" {
		http.Redirect(w, r, targetURL, http.StatusFound)
		return
	}

	start, end, openEnded, err := parseRangeSmart(rangeHeader)
	if err != nil {
		http.Redirect(w, r, targetURL, http.StatusFound)
		return
	}

	// workers.dev/r2.dev: relay directly with native 206
	if !strings.Contains(targetURL, "googleusercontent.com") {
		relayUpstream(targetURL, rangeHeader, w, r)
		return
	}

	// Google CDN: open connection, skip to offset, pipe directly to client
	// Use request context so download stops when client disconnects
	ctx := r.Context()

	req, _ := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Referer", "https://gamerxyt.com/")

	resp, err := proxyClient.Do(req)
	if err != nil {
		http.Error(w, `{"error":"upstream failed"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		http.Error(w, string(body), resp.StatusCode)
		return
	}

	totalSize := resp.ContentLength
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "video/mkv"
	}

	// Skip to requested offset
	if start > 0 {
		skipped, skipErr := skipBytes(resp.Body, start)
		if skipErr != nil {
			log.Printf("[proxy] skip error at %d: %v", skipped, skipErr)
			http.Error(w, `{"error":"skip failed"}`, http.StatusBadGateway)
			return
		}
	}

	// Calculate serve length
	var serveLen int64
	if !openEnded && end >= start {
		serveLen = end - start + 1
	} else if totalSize > 0 {
		serveLen = totalSize - start
	} else {
		serveLen = -1
	}

	// Write 206 headers
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Accept-Ranges", "bytes")
	if serveLen > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+serveLen-1, totalSize))
		w.Header().Set("Content-Length", strconv.FormatInt(serveLen, 10))
	} else if totalSize > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, totalSize-1, totalSize))
	}
	w.WriteHeader(http.StatusPartialContent)

	// Pipe directly to client — no intermediate buffer, maximum throughput
	// When client disconnects, w.Write fails and we return immediately
	var src io.Reader = resp.Body
	if serveLen > 0 {
		src = io.LimitReader(resp.Body, serveLen)
	}

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 256*1024)
	total := int64(0)

	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			written, writeErr := w.Write(buf[:n])
			total += int64(written)
			if canFlush {
				flusher.Flush()
			}
			if writeErr != nil {
				log.Printf("[proxy] client disconnected after %d bytes", total)
				return
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				log.Printf("[proxy] upstream read error after %d bytes: %v", total, readErr)
			}
			break
		}
	}

	log.Printf("[proxy] served %d bytes from offset %d", total, start)
}

func relayUpstream(targetURL, rangeHeader string, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	req, _ := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://gamerxyt.com/")
	req.Header.Set("Range", rangeHeader)

	resp, err := proxyClient.Do(req)
	if err != nil {
		http.Error(w, `{"error":"upstream failed"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		http.Error(w, string(body), resp.StatusCode)
		return
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 256*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
}

func skipBytes(r io.Reader, n int64) (int64, error) {
	var skipped int64
	for skipped < n {
		batch := n - skipped
		if batch > 4*1024*1024 {
			batch = 4 * 1024 * 1024
		}
		written, err := io.CopyN(io.Discard, r, batch)
		skipped += written
		if err != nil {
			return skipped, err
		}
	}
	return skipped, nil
}

func parseRangeSmart(rangeHeader string) (start, end int64, openEnded bool, err error) {
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, 0, false, fmt.Errorf("invalid range scheme")
	}
	rangeVal := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.SplitN(rangeVal, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false, fmt.Errorf("invalid range format")
	}
	if parts[0] == "" {
		return 0, 0, false, fmt.Errorf("suffix range")
	}
	start, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false, fmt.Errorf("invalid start")
	}
	if parts[1] == "" {
		return start, -1, true, nil
	}
	end, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, false, fmt.Errorf("invalid end")
	}
	return start, end, false, nil
}

// --- Pre-warming ---

// prewarmSem limits concurrent pre-warm goroutines to avoid
// overwhelming upstream hub/gamerxyt servers.
var prewarmSem = make(chan struct{}, 5)

func prewarmExtract(ctx context.Context, hubURL string) {
	prewarmSem <- struct{}{}        // acquire slot
	defer func() { <-prewarmSem }() // release slot
	cacheKey := "/extract/?url=" + hubURL
	if cached, ok := cache.Get(cacheKey); ok {
		var existing string
		if json.Unmarshal(cached, &existing) == nil && existing != "" {
			return
		}
	}

	prewarmCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	redURL, err := resolveRedButtonFromHub(prewarmCtx, hubURL)
	if err != nil || redURL == "" {
		return
	}

	googleURL, err := resolveRedButton(prewarmCtx, redURL)
	if err != nil || googleURL == "" {
		return
	}

	if data, err := json.Marshal(googleURL); err == nil {
		cache.Set(cacheKey, data)
		log.Printf("[prewarm] cached CDN URL")
	}
}
