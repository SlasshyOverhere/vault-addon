package main

import (
	"context"
	"encoding/json"
	"fmt"

	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// --- Domain discovery ---

var (
	activeDomain   string
	activeDomainMu sync.RWMutex
)

func findActiveDomain(ctx context.Context) string {
	activeDomainMu.RLock()
	if activeDomain != "" {
		activeDomainMu.RUnlock()
		return activeDomain
	}
	activeDomainMu.RUnlock()

	domains := getSiteDomains()
	if len(domains) == 0 {
		return ""
	}

	// Parallel health check across all domains
	type result struct {
		domain string
		ok     bool
	}
	ch := make(chan result, len(domains))
	var wg sync.WaitGroup
	for _, d := range domains {
		wg.Add(1)
		go func(domain string) {
			defer wg.Done()
			dCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			reqURL := domain + "/?s=test"
			resp, err := httpGet(dCtx, reqURL, nil)
			if err != nil {
				ch <- result{domain, false}
				return
			}
			resp.Body.Close()
			ch <- result{domain, resp.StatusCode < 400}
		}(d)
	}
	wg.Wait()
	close(ch)

	firstAlive := ""
	for r := range ch {
		if r.ok && firstAlive == "" {
			firstAlive = r.domain
		}
	}

	if firstAlive == "" && len(domains) > 0 {
		firstAlive = domains[0]
	}

	if firstAlive != "" {
		activeDomainMu.Lock()
		activeDomain = firstAlive
		activeDomainMu.Unlock()
	}
	return firstAlive
}

// invalidateActiveDomain clears the cached active domain so the next
// request re-discovers. Called when sites are added/removed.
func invalidateActiveDomain() {
	activeDomainMu.Lock()
	activeDomain = ""
	activeDomainMu.Unlock()
}

// --- Meta fetching ---

type Meta struct {
	Name string
	Year int
}

var cinemetaClient = newHTTPClient(8)

// cinemetaCache caches Cinemeta responses (names don't change)
var (
	cinemetaCache   = make(map[string]*Meta)
	cinemetaCacheMu sync.RWMutex
)

func getMetaCached(ctx context.Context, id, itype string) (*Meta, error) {
	cacheKey := itype + ":" + id
	cinemetaCacheMu.RLock()
	if m, ok := cinemetaCache[cacheKey]; ok {
		cinemetaCacheMu.RUnlock()
		return m, nil
	}
	cinemetaCacheMu.RUnlock()

	meta, err := getMeta(ctx, id, itype)
	if err != nil {
		return nil, err
	}
	if meta != nil && meta.Name != "" {
		cinemetaCacheMu.Lock()
		cinemetaCache[cacheKey] = meta
		cinemetaCacheMu.Unlock()
	}
	return meta, err
}

func getMetaFromCinemeta(ctx context.Context, imdbID, itype string) (*Meta, error) {
	url := fmt.Sprintf("https://v3-cinemeta.strem.io/meta/%s/%s.json", itype, imdbID)
	resp, err := httpGetWithClient(ctx, url, nil, cinemetaClient)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cinemeta %d", resp.StatusCode)
	}

	var data struct {
		Meta struct {
			Name        string `json:"name"`
			Year        string `json:"year"`
			ReleaseInfo string `json:"releaseInfo"`
		} `json:"meta"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	year := 0
	if data.Meta.Year != "" {
		year, _ = strconv.Atoi(data.Meta.Year)
	}
	if year == 0 && data.Meta.ReleaseInfo != "" {
		year, _ = strconv.Atoi(data.Meta.ReleaseInfo)
	}

	return &Meta{Name: data.Meta.Name, Year: year}, nil
}

var tmdbTitleRe = regexp.MustCompile(`<title>([^(]+)\((?:TV Series )?(\d{4})`)

func getNameAndYearFromTmdbPage(ctx context.Context, tmdbNum int, isSeries bool) (*Meta, error) {
	typeSlug := "movie"
	if isSeries {
		typeSlug = "tv"
	}
	url := fmt.Sprintf("https://www.themoviedb.org/%s/%d", typeSlug, tmdbNum)

	resp, err := httpGet(ctx, url, map[string]string{
		"User-Agent": browserUA,
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("tmdb %d", resp.StatusCode)
	}

	// Read only first 2KB to find title
	buf := make([]byte, 2048)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])

	m := tmdbTitleRe.FindStringSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("no title found")
	}

	year, _ := strconv.Atoi(m[2])
	return &Meta{Name: strings.TrimSpace(m[1]), Year: year}, nil
}

func getMeta(ctx context.Context, id, itype string) (*Meta, error) {
	if strings.HasPrefix(id, "tt") {
		imdbID := strings.Split(id, ":")[0]
		return getMetaFromCinemeta(ctx, imdbID, itype)
	}
	if strings.HasPrefix(id, "tmdb:") {
		parts := strings.Split(strings.TrimPrefix(id, "tmdb:"), ":")
		tmdbNum, _ := strconv.Atoi(parts[0])
		isSeries := itype == "series" && len(parts) >= 2
		return getNameAndYearFromTmdbPage(ctx, tmdbNum, isSeries)
	}
	return nil, fmt.Errorf("unknown id format")
}

// --- Page search cache ---

var (
	pageURLCache   = make(map[string]pageURLCacheEntry)
	pageURLCacheMu sync.RWMutex
)

type pageURLCacheEntry struct {
	url      string
	expires  time.Time
}

func findPageURLCached(ctx context.Context, name string, year int, isSeries bool) (string, error) {
	typeSlug := "-movie-"
	if isSeries {
		typeSlug = "-series-"
	}
	cacheKey := fmt.Sprintf("%s|%d|%s", strings.ToLower(name), year, typeSlug)

	pageURLCacheMu.RLock()
	if e, ok := pageURLCache[cacheKey]; ok && time.Now().Before(e.expires) {
		pageURLCacheMu.RUnlock()
		if e.url != "" {
			return e.url, nil
		}
		return "", fmt.Errorf("cached not found")
	}
	pageURLCacheMu.RUnlock()

	url, err := findPageURL(ctx, name, year, isSeries)
	entry := pageURLCacheEntry{url: url, expires: time.Now().Add(30 * time.Minute)}
	if err != nil {
		entry.url = ""
	}
	pageURLCacheMu.Lock()
	pageURLCache[cacheKey] = entry
	pageURLCacheMu.Unlock()
	return url, err
}

// Page HTML cache (15 min TTL, shared across requests for same show)
var (
	pageHTMLCache   = make(map[string]pageHTMLCacheEntry)
	pageHTMLCacheMu sync.RWMutex
)

type pageHTMLCacheEntry struct {
	html    string
	expires time.Time
}

func fetchPageHTMLCached(ctx context.Context, url string) string {
	pageHTMLCacheMu.RLock()
	if e, ok := pageHTMLCache[url]; ok && time.Now().Before(e.expires) {
		pageHTMLCacheMu.RUnlock()
		return e.html
	}
	pageHTMLCacheMu.RUnlock()

	html, err := fetchPageHTML(ctx, url)
	if err != nil || html == "" {
		return ""
	}

	pageHTMLCacheMu.Lock()
	pageHTMLCache[url] = pageHTMLCacheEntry{
		html:    html,
		expires: time.Now().Add(15 * time.Minute),
	}
	pageHTMLCacheMu.Unlock()
	return html
}

// --- Page search ---

func findPageURL(ctx context.Context, name string, year int, isSeries bool) (string, error) {
	base := findActiveDomain(ctx)
	searchURL := fmt.Sprintf("%s/?s=%s", base, urlEncode(name))

	resp, err := httpGet(ctx, searchURL, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", err
	}

	typeSlug := "-movie-"
	if isSeries {
		typeSlug = "-series-"
	}

	namePrefix := strings.ToLower(name)
	if len(namePrefix) > 8 {
		namePrefix = namePrefix[:8]
	}

	var result string
	doc.Find(".movie-card").EachWithBreak(func(_ int, card *goquery.Selection) bool {
		href, _ := card.Attr("href")
		if !strings.Contains(href, typeSlug) {
			return true
		}

		metaText := card.Find(".movie-card-meta").Text()
		cardYear, _ := strconv.Atoi(strings.TrimSpace(metaText))
		if year > 0 && cardYear > 0 && math.Abs(float64(cardYear-year)) > 1 {
			return true
		}

		cardTitle := strings.TrimSpace(regexp.MustCompile(`\[.*?\]`).ReplaceAllString(card.Find(".movie-card-title").Text(), ""))
		if !strings.Contains(strings.ToLower(cardTitle), namePrefix) {
			return true
		}

		if strings.HasPrefix(href, "http") {
			result = href
		} else {
			result = base + href
		}
		return false
	})

	return result, nil
}

// --- Stream extraction ---

var (
	heightRe = regexp.MustCompile(`\b(\d{3,})p\b`)
	sizeRe   = regexp.MustCompile(`([\d.]+)\s*(GB|MB|TB)`)
	seRe     = regexp.MustCompile(`(?i)S(\d{1,2})E(\d{1,2})`)
)

var sizeMultipliers = map[string]float64{
	"TB": 1024 * 1024 * 1024 * 1024,
	"GB": 1024 * 1024 * 1024,
	"MB": 1024 * 1024,
}

func parseSizeBytes(sizeStr string) float64 {
	m := sizeRe.FindStringSubmatch(sizeStr)
	if m == nil {
		return 0
	}
	val, _ := strconv.ParseFloat(m[1], 64)
	mult := sizeMultipliers[strings.ToUpper(m[2])]
	return val * mult
}

func extractHubURLs(item *goquery.Selection, seen map[string]bool) []string {
	var urls []string
	item.Find("a").Each(func(_ int, a *goquery.Selection) {
		href, _ := a.Attr("href")
		if href == "" {
			return
		}
		if !regexp.MustCompile(`(?i)hubcloud|hubdrive|gpdl\.hubcdn`).MatchString(href) {
			return
		}
		if regexp.MustCompile(`(?i)pixel\.hubcdn|pixel\.rohitkiskk`).MatchString(href) {
			return
		}
		clean := strings.Split(href, "?")[0]
		if !seen[clean] {
			seen[clean] = true
			urls = append(urls, href)
		}
	})
	return urls
}

func streamURL(url, baseURL string) string {
	if strings.Contains(url, "hubdrive") {
		return url
	}
	return baseURL + "/extract/?url=" + urlEncode(url)
}

func buildStreamTitle(fileTitle, sizeStr, url string) string {
	host := "HubCloud"
	if strings.Contains(url, "hubdrive") {
		host = "HubDrive"
	}
	sizeDisplay := "?"
	if m := sizeRe.FindString(sizeStr); m != "" {
		sizeDisplay = m
	}
	return fileTitle + "\n" + "\U0001F4BF " + sizeDisplay + " | " + host
}

// resolveAndValidateBlue fetches blue button links from a hub URL,
// validates each with a HEAD request, and returns only the active ones.
func resolveAndValidateBlue(ctx context.Context, hubURL string) []blueLink {
	blueCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	links, err := resolveBlueButtons(blueCtx, hubURL)
	if err != nil || len(links) == 0 {
		return nil
	}

	var valid []blueLink
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, l := range links {
		wg.Add(1)
		go func(bl blueLink) {
			defer wg.Done()
			if validateBlueLink(blueCtx, bl.url) {
				mu.Lock()
				valid = append(valid, bl)
				mu.Unlock()
			}
		}(l)
	}
	wg.Wait()
	return valid
}

// blueButtonCache caches resolved blue button results per hub URL
var (
	blueButtonCache   = make(map[string]blueCacheEntry)
	blueButtonCacheMu sync.RWMutex
)

type blueCacheEntry struct {
	links   []blueLink
	expires time.Time
}

func resolveBlueCached(ctx context.Context, hubURL string) []blueLink {
	blueButtonCacheMu.RLock()
	if e, ok := blueButtonCache[hubURL]; ok && time.Now().Before(e.expires) {
		blueButtonCacheMu.RUnlock()
		return e.links
	}
	blueButtonCacheMu.RUnlock()

	links := resolveAndValidateBlue(ctx, hubURL)
	blueButtonCacheMu.Lock()
	blueButtonCache[hubURL] = blueCacheEntry{
		links:   links,
		expires: time.Now().Add(5 * time.Minute),
	}
	blueButtonCacheMu.Unlock()
	return links
}

func extractStreams(ctx context.Context, html string, episodeFilter func(string) bool, displayName, baseURL string) []Stream {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil
	}

	// Phase 1: Collect all hub URLs and their metadata (fast, no HTTP)
	type hubEntry struct {
		hubURL     string
		streamName string
		fileTitle  string
		sizeBytes  float64
	}
	var entries []hubEntry
	var results []Stream
	seen := make(map[string]bool)

	doc.Find(".season-item.episode-item").Each(func(_ int, group *goquery.Selection) {
		epNumText := group.Find(".episode-number").Text()
		if episodeFilter != nil && !episodeFilter(epNumText) {
			return
		}

		group.Find(".episode-download-item").Each(func(_ int, item *goquery.Selection) {
			fileTitle := strings.TrimSpace(item.Find(".episode-file-title").Text())
			itemText := item.Text()

			sizeMatch := sizeRe.FindString(itemText)
			heightMatch := heightRe.FindStringSubmatch(fileTitle)
			height := 0
			if heightMatch != nil {
				height, _ = strconv.Atoi(heightMatch[1])
			}
			hubURLs := extractHubURLs(item, seen)
			sizeBytes := parseSizeBytes(sizeMatch)

			seMatch := seRe.FindStringSubmatch(fileTitle)
			se := ""
			if seMatch != nil {
				se = fmt.Sprintf("S%sE%s", seMatch[1], seMatch[2])
			}

			parts := []string{displayName}
			if se != "" {
				parts = append(parts, se)
			}
			if height > 0 {
				parts = append(parts, fmt.Sprintf("%dp", height))
			}
			if sizeMatch != "" {
				parts = append(parts, sizeMatch)
			}
			streamName := strings.Join(parts, " ")
			if streamName == "" {
				streamName = "Stream"
			}

			for _, u := range hubURLs {
				// Red button — always shown via /extract/ (no HTTP needed)
				results = append(results, Stream{
					URL:   streamURL(u, baseURL),
					Name:  streamName,
					Title: buildStreamTitle(fileTitle, sizeMatch, u),
					BehaviorHints: map[string]any{
						"notWebReady": true,
						"videoSize":   sizeBytes,
					},
				})

				// Collect for concurrent blue-button resolution
				entries = append(entries, hubEntry{
					hubURL:     u,
					streamName: streamName,
					fileTitle:  fileTitle,
					sizeBytes:  sizeBytes,
				})
			}
		})
	})

	// Phase 2: Resolve blue buttons concurrently (all hub URLs in parallel)
	if len(entries) > 0 {
		type resolvedBlue struct {
			entry     hubEntry
			blueLinks []blueLink
		}
		ch := make(chan resolvedBlue, len(entries))
		for _, e := range entries {
			go func(ent hubEntry) {
				links := resolveBlueCached(ctx, ent.hubURL)
				ch <- resolvedBlue{entry: ent, blueLinks: links}
			}(e)
		}
		for i := 0; i < len(entries); i++ {
			r := <-ch
			for _, bl := range r.blueLinks {
				results = append(results, Stream{
					URL:   bl.url,
					Name:  r.entry.streamName + " [" + bl.label + "]",
					Title: r.entry.fileTitle + "\n🔵 " + bl.label + " | Direct",
					BehaviorHints: map[string]any{
						"notWebReady": true,
						"videoSize":   r.entry.sizeBytes,
					},
				})
			}
		}
	}

	return results
}

func extractMovieStreams(ctx context.Context, html, displayName, baseURL string) []Stream {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil
	}

	// Phase 1: Collect all hub URLs and their metadata (fast, no HTTP)
	type hubEntry struct {
		hubURL     string
		streamName string
		fileTitle  string
		sizeBytes  float64
	}
	var entries []hubEntry
	var results []Stream
	seen := make(map[string]bool)

	doc.Find(".download-item").Each(func(_ int, item *goquery.Selection) {
		fileTitle := strings.TrimSpace(item.Find(".file-title").Text())
		itemText := item.Text()

		sizeMatch := sizeRe.FindString(itemText)
		heightMatch := heightRe.FindStringSubmatch(fileTitle)
		height := 0
		if heightMatch != nil {
			height, _ = strconv.Atoi(heightMatch[1])
		}
		hubURLs := extractHubURLs(item, seen)
		sizeBytes := parseSizeBytes(sizeMatch)

		name := displayName
		if name == "" {
			if height > 0 {
				name = fmt.Sprintf("%dp", height)
			} else {
				name = "Stream"
			}
		}

		for _, u := range hubURLs {
			// Red button — always shown via /extract/
			results = append(results, Stream{
				URL:   streamURL(u, baseURL),
				Name:  name,
				Title: buildStreamTitle(fileTitle, sizeMatch, u),
				BehaviorHints: map[string]any{
					"notWebReady": true,
					"videoSize":   sizeBytes,
				},
			})

			entries = append(entries, hubEntry{
				hubURL:     u,
				streamName: name,
				fileTitle:  fileTitle,
				sizeBytes:  sizeBytes,
			})
		}
	})

	// Phase 2: Resolve blue buttons CONCURRENTLY (all hub URLs in parallel)
	if len(entries) > 0 {
		type resolvedBlue struct {
			entry     hubEntry
			blueLinks []blueLink
		}
		ch := make(chan resolvedBlue, len(entries))
		for _, e := range entries {
			go func(ent hubEntry) {
				links := resolveBlueCached(ctx, ent.hubURL)
				ch <- resolvedBlue{entry: ent, blueLinks: links}
			}(e)
		}
		for i := 0; i < len(entries); i++ {
			r := <-ch
			for _, bl := range r.blueLinks {
				results = append(results, Stream{
					URL:   bl.url,
					Name:  r.entry.streamName + " [" + bl.label + "]",
					Title: r.entry.fileTitle + "\n🔵 " + bl.label + " | Direct",
					BehaviorHints: map[string]any{
						"notWebReady": true,
						"videoSize":   r.entry.sizeBytes,
					},
				})
			}
		}
	}

	return results
}

func extractPackStreams(ctx context.Context, html string, seasonNum int, displayName, baseURL string) ([]Stream, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}

	sp := fmt.Sprintf("S%02d", seasonNum)
	seen := make(map[string]bool)

	type packItem struct {
		url        string
		streamName string
		title      string
		sizeBytes  float64
		fileTitle  string
	}
	var items []packItem

	doc.Find("#complete-pack .download-item").Each(func(_ int, item *goquery.Selection) {
		epNum := strings.TrimSpace(item.Find(".episode-number").Text())
		if !strings.Contains(epNum, sp) {
			return
		}

		fileTitle := strings.TrimSpace(item.Find(".file-title").Text())
		itemText := item.Text()

		sizeMatch := sizeRe.FindString(itemText)
		heightMatch := heightRe.FindStringSubmatch(fileTitle)
		height := 0
		if heightMatch != nil {
			height, _ = strconv.Atoi(heightMatch[1])
		}
		hubURLs := extractHubURLs(item, seen)
		sizeBytes := parseSizeBytes(sizeMatch)

		parts := []string{displayName, sp}
		if height > 0 {
			parts = append(parts, fmt.Sprintf("%dp", height))
		}
		if sizeMatch != "" {
			parts = append(parts, sizeMatch)
		}
		streamName := strings.Join(parts, " ")
		if streamName == "" {
			streamName = "Pack"
		}

		for _, u := range hubURLs {
			items = append(items, packItem{
				url:        u,
				streamName: streamName,
				title:      buildStreamTitle(fileTitle, sizeMatch, u),
				sizeBytes:  sizeBytes,
				fileTitle:  fileTitle,
			})
		}
	})

	// Resolve hubcloud URLs concurrently
	var mu sync.Mutex
	var allStreams []Stream
	var wg sync.WaitGroup

	for _, item := range items {
		wg.Add(1)
		go func(it packItem) {
			defer wg.Done()

			if strings.Contains(it.url, "hubdrive") {
				mu.Lock()
				allStreams = append(allStreams, Stream{
					URL:   it.url,
					Name:  it.streamName,
					Title: it.title,
					BehaviorHints: map[string]any{
						"notWebReady": true,
						"videoSize":   it.sizeBytes,
					},
				})
				mu.Unlock()
				return
			}

			// Use cached pack link resolution
			links, err := resolvePackLinksCached(ctx, it.url)
			if err != nil || len(links) == 0 {
				mu.Lock()
				allStreams = append(allStreams, Stream{
					URL:   it.url,
					Name:  it.streamName,
					Title: it.title,
					BehaviorHints: map[string]any{
						"notWebReady": true,
						"videoSize":   it.sizeBytes,
					},
				})
				mu.Unlock()
				return
			}

			mu.Lock()
			for _, l := range links {
				sizeDisplay := "?"
				if it.sizeBytes > 0 {
					sizeDisplay = fmt.Sprintf("%.2f GB", it.sizeBytes/(1024*1024*1024))
				}
				allStreams = append(allStreams, Stream{
					URL:  l.url,
					Name: it.streamName,
					Title: fmt.Sprintf("%s\n💿 %s | %s", it.fileTitle, sizeDisplay, l.label),
					BehaviorHints: map[string]any{
						"notWebReady": true,
						"videoSize":   it.sizeBytes,
					},
				})
			}
			mu.Unlock()
		}(item)
	}

	wg.Wait()
	return allStreams, nil
}

// Pack link resolution cache
var (
	packLinkCache   = make(map[string]packCacheEntry)
	packLinkCacheMu sync.RWMutex
)

type packCacheEntry struct {
	links   []packLink
	expires time.Time
}

func resolvePackLinksCached(ctx context.Context, hubURL string) ([]packLink, error) {
	packLinkCacheMu.RLock()
	if e, ok := packLinkCache[hubURL]; ok && time.Now().Before(e.expires) {
		packLinkCacheMu.RUnlock()
		return e.links, nil
	}
	packLinkCacheMu.RUnlock()

	links, err := resolvePackLinks(ctx, hubURL)
	packLinkCacheMu.Lock()
	packLinkCache[hubURL] = packCacheEntry{
		links:   links,
		expires: time.Now().Add(5 * time.Minute),
	}
	packLinkCacheMu.Unlock()
	return links, err
}
