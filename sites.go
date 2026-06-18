package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Site represents a registered content source site.
type Site struct {
	URL     string `json:"url"`
	Name    string `json:"name"`
	AddedAt string `json:"addedAt"`
	Enabled bool   `json:"enabled"`
}

type siteFile struct {
	Sites []Site `json:"sites"`
}

var (
	sitesCache []Site
	sitesMu    sync.RWMutex
)

// Default domains to seed on first run (current hardcoded 4khdhub mirrors).
var defaultDomains = []string{
	"https://4khdhub.one",
	"https://4khdhub.link",
	"https://4khdhub.click",
	"https://4khdhub.ink",
	"https://4khdhub.to",
	"https://4khdhub.cc",
}

func sitesFilePath() string {
	return filepath.Join(configDir(), "sites.json")
}

// initSiteRegistry loads the site registry from disk.
// If the file doesn't exist, it seeds with the default domains.
func initSiteRegistry() error {
	path := sitesFilePath()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// First run: seed with defaults
			log.Printf("[sites] no sites.json found, seeding with %d default domains", len(defaultDomains))
			sites := make([]Site, 0, len(defaultDomains))
			now := time.Now().UTC().Format(time.RFC3339)
			for _, d := range defaultDomains {
				sites = append(sites, Site{
					URL:     d,
					Name:    siteNameFromURL(d),
					AddedAt: now,
					Enabled: true,
				})
			}
			sitesMu.Lock()
			sitesCache = sites
			sitesMu.Unlock()
			return saveSites(sites)
		}
		return fmt.Errorf("reading sites.json: %w", err)
	}

	var sf siteFile
	if err := json.Unmarshal(data, &sf); err != nil {
		log.Printf("[sites] corrupt sites.json, starting empty: %v", err)
		sitesMu.Lock()
		sitesCache = []Site{}
		sitesMu.Unlock()
		return saveSites([]Site{})
	}

	sitesMu.Lock()
	sitesCache = sf.Sites
	sitesMu.Unlock()
	log.Printf("[sites] loaded %d sites", len(sf.Sites))
	return nil
}

// loadSites reads sites from the in-memory cache.
func loadSites() []Site {
	sitesMu.RLock()
	defer sitesMu.RUnlock()
	cp := make([]Site, len(sitesCache))
	copy(cp, sitesCache)
	return cp
}

// saveSites writes the site list to disk (atomic: write to .tmp, rename).
func saveSites(sites []Site) error {
	path := sitesFilePath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	sf := siteFile{Sites: sites}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling sites: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("writing sites.tmp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming sites file: %w", err)
	}
	return nil
}

// getSites returns all registered sites.
func getSites() []Site {
	return loadSites()
}

// getSiteDomains returns just the URL strings for use by findActiveDomain.
func getSiteDomains() []string {
	sites := loadSites()
	domains := make([]string, 0, len(sites))
	for _, s := range sites {
		if s.Enabled {
			domains = append(domains, s.URL)
		}
	}
	return domains
}

// addSite validates and registers a new site.
func addSite(rawURL string) (*Site, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("URL is required")
	}

	// Parse and normalize
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("URL must use http or https")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("URL must have a host")
	}

	// Normalize to origin only
	normalized := fmt.Sprintf("%s://%s", u.Scheme, strings.ToLower(u.Host))
	// Strip trailing slash
	normalized = strings.TrimSuffix(normalized, "/")

	// Duplicate check
	sitesMu.RLock()
	for _, s := range sitesCache {
		if strings.EqualFold(s.URL, normalized) {
			sitesMu.RUnlock()
			return nil, fmt.Errorf("site already registered")
		}
	}
	sitesMu.RUnlock()

	site := Site{
		URL:     normalized,
		Name:    siteNameFromURL(normalized),
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Enabled: true,
	}

	// Persist
	sitesMu.Lock()
	sitesCache = append(sitesCache, site)
	if err := saveSites(sitesCache); err != nil {
		// Rollback
		sitesCache = sitesCache[:len(sitesCache)-1]
		sitesMu.Unlock()
		return nil, fmt.Errorf("saving sites: %w", err)
	}
	sitesMu.Unlock()

	// Invalidate active domain cache so next request re-discovers
	invalidateActiveDomain()

	log.Printf("[sites] added %s", normalized)
	return &site, nil
}

// removeSite removes a site by URL.
func removeSite(rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)

	sitesMu.Lock()
	defer sitesMu.Unlock()

	for i, s := range sitesCache {
		if s.URL == rawURL {
			sitesCache = append(sitesCache[:i], sitesCache[i+1:]...)
			if err := saveSites(sitesCache); err != nil {
				// Rollback: re-insert at position
				sitesCache = append(sitesCache[:i], append([]Site{s}, sitesCache[i:]...)...)
				return fmt.Errorf("saving sites: %w", err)
			}
			invalidateActiveDomain()
			log.Printf("[sites] removed %s", rawURL)
			return nil
		}
	}
	return fmt.Errorf("site not found")
}

// siteNameFromURL extracts a display name from a URL.
func siteNameFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return strings.ToLower(u.Host)
}
