package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	cacheTTL         = 15 * time.Minute
	cachePruneInterval = 1 * time.Minute
	cacheFileName    = "cache.json"
)

type cacheEntry struct {
	Data      json.RawMessage `json:"data"`
	CachedAt  time.Time       `json:"cached_at"`
}

type responseCache struct {
	mu      sync.RWMutex
	entries map[string]cacheEntry
	dirty   bool
	saveTimer *time.Timer
}

var cache = &responseCache{
	entries: make(map[string]cacheEntry),
}

// scheduleSaveLocked starts a 200ms debounce timer to coalesce
// rapid Set calls into a single disk write. Must be called with c.mu held.
func (c *responseCache) scheduleSaveLocked() {
	if c.saveTimer != nil {
		c.saveTimer.Stop()
	}
	c.saveTimer = time.AfterFunc(200*time.Millisecond, c.saveToDisk)
}

func (c *responseCache) Get(key string) (json.RawMessage, bool) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok {
		return nil, false
	}
	if time.Since(entry.CachedAt) > cacheTTL {
		// Expired — remove lazily
		c.mu.Lock()
		delete(c.entries, key)
		c.dirty = true
		c.mu.Unlock()
		return nil, false
	}
	return entry.Data, true
}

func (c *responseCache) Set(key string, data json.RawMessage) {
	c.mu.Lock()
	c.entries[key] = cacheEntry{
		Data:     data,
		CachedAt: time.Now(),
	}
	c.dirty = true
	c.scheduleSaveLocked()
	c.mu.Unlock()
}

func (c *responseCache) prune() {
	c.mu.Lock()
	defer c.mu.Unlock()

	pruned := 0
	for k, e := range c.entries {
		if time.Since(e.CachedAt) > cacheTTL {
			delete(c.entries, k)
			pruned++
		}
	}
	if pruned > 0 {
		c.dirty = true
		log.Printf("[cache] pruned %d expired entries", pruned)
	}
}

func cacheFilePath() string {
	dir := configDir()
	return filepath.Join(dir, cacheFileName)
}

func (c *responseCache) loadFromDisk() {
	path := cacheFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return // No cache file yet, that's fine
	}

	var entries map[string]cacheEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Printf("[cache] corrupt cache file, starting fresh: %v", err)
		return
	}

	// Prune expired on load
	loaded := 0
	for k, e := range entries {
		if time.Since(e.CachedAt) <= cacheTTL {
			c.entries[k] = e
			loaded++
		}
	}
	log.Printf("[cache] loaded %d entries from disk", loaded)
}

func (c *responseCache) saveToDisk() {
	c.mu.RLock()
	if !c.dirty {
		c.mu.RUnlock()
		return
	}
	// Snapshot under read lock
	snapshot := make(map[string]cacheEntry, len(c.entries))
	for k, v := range c.entries {
		snapshot[k] = v
	}
	c.mu.RUnlock()

	data, err := json.Marshal(snapshot)
	if err != nil {
		log.Printf("[cache] failed to marshal: %v", err)
		return
	}

	dir := configDir()
	os.MkdirAll(dir, 0755)
	path := cacheFilePath()

	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("[cache] failed to write: %v", err)
		return
	}

	c.mu.Lock()
	c.dirty = false
	c.mu.Unlock()
}

func (c *responseCache) Clear() {
	c.mu.Lock()
	c.entries = make(map[string]cacheEntry)
	c.dirty = true
	c.scheduleSaveLocked()
	c.mu.Unlock()
	log.Println("[cache] cleared")
}

// startCachePruner runs periodic cache pruning in the background.
// Returns a stop function.
func startCachePruner() func() {
	ticker := time.NewTicker(cachePruneInterval)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				cache.prune()
			case <-done:
				ticker.Stop()
				return
			}
		}
	}()
	return func() { close(done) }
}

// cacheKeyForRequest builds a cache key from the request path and query.
func cacheKeyForRequest(path, rawQuery string) string {
	if rawQuery == "" {
		return path
	}
	// Normalize: sort query params for consistent keys
	return path + "?" + rawQuery
}

// handleCachedStream wraps a handler with cache GET/SET.
// Returns true if cache hit (response already written), false if caller should proceed.
func handleCachedStream(w http.ResponseWriter, r *http.Request) bool {
	key := cacheKeyForRequest(r.URL.Path, r.URL.RawQuery)
	if data, ok := cache.Get(key); ok {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "HIT")
		w.Write(data)
		return true
	}
	return false
}

// cacheStoreResponse stores a JSON response in the cache.
// Use after encoding the response.
func cacheStoreResponse(key string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	cache.Set(key, data)
}

// invalidateCacheForSite clears the entire cache when sites change.
// Called from site add/remove handlers.
func invalidateCacheForSite() {
	cache.Clear()
}

// isCacheStale checks if cached data for a key is older than maxAge.
// Used for background refresh patterns.
func isCacheStale(key string, maxAge time.Duration) bool {
	cache.mu.RLock()
	entry, ok := cache.entries[key]
	cache.mu.RUnlock()
	if !ok {
		return true
	}
	return time.Since(entry.CachedAt) > maxAge
}

// stripCacheBuster removes cache-busting params that don't affect content.
func stripCacheBuster(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	var parts []string
	for _, p := range strings.Split(rawQuery, "&") {
		if strings.HasPrefix(p, "_=") || strings.HasPrefix(p, "cb=") {
			continue
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "&")
}
