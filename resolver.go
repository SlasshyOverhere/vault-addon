package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// --- HTTP client helpers ---

func newHTTPClient(timeoutSec int) *http.Client {
	return &http.Client{
		Timeout: time.Duration(timeoutSec) * time.Second,
	}
}

var defaultClient = newHTTPClient(15)
var noRedirectClient = &http.Client{
	Timeout: 25 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func httpGet(ctx context.Context, urlStr string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return defaultClient.Do(req)
}

func httpGetWithClient(ctx context.Context, urlStr string, headers map[string]string, client *http.Client) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return client.Do(req)
}

func httpGetNoRedirect(ctx context.Context, urlStr string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "text/html,*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return noRedirectClient.Do(req)
}

func fetchPageHTML(ctx context.Context, url string) (string, error) {
	resp, err := httpGet(ctx, url, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func urlEncode(s string) string {
	return url.QueryEscape(s)
}

// --- CDN cache ---

type cdnCacheEntry struct {
	redURL  string
	expires time.Time
}

var (
	cdnCache   = make(map[string]*cdnCacheEntry)
	cdnCacheMu sync.RWMutex
)

func cdnCacheGet(key string) (string, bool) {
	cdnCacheMu.RLock()
	defer cdnCacheMu.RUnlock()
	e, ok := cdnCache[key]
	if !ok || time.Now().After(e.expires) {
		return "", false
	}
	return e.redURL, true
}

func cdnCacheSet(key, redURL string) {
	cdnCacheMu.Lock()
	defer cdnCacheMu.Unlock()
	cdnCache[key] = &cdnCacheEntry{
		redURL:  redURL,
		expires: time.Now().Add(30 * time.Second),
	}
}

// --- Red button resolution (pixel.hubcloud.cx → Google CDN) ---

var googleCDNRe = regexp.MustCompile(`https://video-downloads\.googleusercontent\.com/[^\s"']+`)

func resolveRedButton(ctx context.Context, pixelURL string) (string, error) {
	log.Printf("  -> resolving red button")
	curURL := pixelURL

	for hop := 0; hop < 5; hop++ {
		log.Printf("  -> red hop %d: %.120s", hop, curURL)

		resp, err := httpGetNoRedirect(ctx, curURL, map[string]string{
			"Referer":         "https://gamerxyt.com/",
			"Cache-Control":   "no-cache, no-store, must-revalidate",
			"Pragma":          "no-cache",
		})
		if err != nil {
			return "", err
		}
		resp.Body.Close()

		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			log.Printf("  -> non-redirect status %d, giving up", resp.StatusCode)
			return "", nil
		}

		loc := resp.Header.Get("Location")
		if loc == "" {
			log.Printf("  -> no location header")
			return "", nil
		}

		// dl.php with link param
		if strings.Contains(loc, "gamerxyt.com/dl.php") {
			log.Printf("  -> found dl.php redirect")
			if u, err := url.Parse(loc); err == nil {
				linkParam := u.Query().Get("link")
				if linkParam != "" {
					if strings.Contains(linkParam, "googleusercontent.com") {
						log.Printf("  -> extracted google CDN URL")
						return linkParam, nil
					}
				}
			}

			// Follow dl.php chain
			gURL, err := followDLChain(ctx, loc)
			if err == nil && gURL != "" {
				return gURL, nil
			}
			return "", nil
		}

		// Direct Google CDN
		if strings.Contains(loc, "googleusercontent.com") {
			log.Printf("  -> direct google CDN URL")
			return loc, nil
		}

		curURL = loc
	}

	log.Printf("  -> max hops reached")
	return "", nil
}

func followDLChain(ctx context.Context, dlURL string) (string, error) {
	cookies := ""
	u := dlURL

	for hop := 0; hop < 5; hop++ {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", browserUA)
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Referer", "https://gamerxyt.com/")
		if cookies != "" {
			req.Header.Set("Cookie", cookies)
		}

		resp, err := noRedirectClient.Do(req)
		if err != nil {
			return "", err
		}

		// Collect cookies
		for _, sc := range resp.Header.Values("Set-Cookie") {
			nameVal := strings.Split(sc, ";")[0]
			name := strings.SplitN(nameVal, "=", 2)[0]
			if !strings.Contains(cookies, name) {
				if cookies != "" {
					cookies += "; "
				}
				cookies += nameVal
			}
		}

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if loc == "" {
				return "", nil
			}
			if strings.Contains(loc, "googleusercontent.com") {
				log.Printf("  -> dl.php redirected to google CDN")
				return loc, nil
			}
			u = loc
			continue
		}

		// Check body for embedded Google CDN URL
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if m := googleCDNRe.FindString(string(body)); m != "" {
			log.Printf("  -> extracted google CDN from dl.php body")
			return m, nil
		}
		return "", nil
	}

	return "", nil
}

// --- Resolve hubcloud red button from hub page ---

var gamerxytRe = regexp.MustCompile(`https://gamerxyt\.com/hubcloud\.php[^'"]+`)
var redBtnRe = regexp.MustCompile(`href="(https://pixel\.hubcloud\.cx/\?id=[^"']+)`)

func resolveRedButtonFromHub(ctx context.Context, hubURL string) (string, error) {
	if cached, ok := cdnCacheGet(hubURL); ok {
		log.Printf("  -> using cached red button URL")
		return cached, nil
	}

	pageHTML, err := fetchPageHTML(ctx, hubURL)
	if err != nil || pageHTML == "" {
		return "", err
	}

	gxytMatch := gamerxytRe.FindString(pageHTML)
	if gxytMatch == "" {
		log.Printf("  -> no gamerxyt link found")
		return "", nil
	}

	log.Printf("  -> gamerxyt found")
	gxytHTML, err := fetchPageHTML(ctx, gxytMatch)
	if err != nil || gxytHTML == "" {
		log.Printf("  -> failed to fetch gamerxyt page")
		return "", nil
	}

	redMatch := redBtnRe.FindStringSubmatch(gxytHTML)
	if redMatch == nil {
		log.Printf("  -> redMatch: no")
		return "", nil
	}
	log.Printf("  -> redMatch: yes")

	cdnCacheSet(hubURL, redMatch[1])
	return redMatch[1], nil
}

// --- Blue button resolution (r2.dev / workers.dev — no pixeldrain) ---

type blueLink struct {
	url   string
	label string
}

// resolveBlueButtons fetches the hub page, finds the gamerxyt page,
// and extracts blue button links (r2.dev, workers.dev).
// Pixeldrain is excluded.
func resolveBlueButtons(ctx context.Context, hubURL string) ([]blueLink, error) {
	pageHTML, err := fetchPageHTML(ctx, hubURL)
	if err != nil || pageHTML == "" {
		return nil, err
	}

	gxytMatch := gamerxytRe.FindString(pageHTML)
	if gxytMatch == "" {
		return nil, nil
	}

	gxytHTML, err := fetchPageHTML(ctx, gxytMatch)
	if err != nil || gxytHTML == "" {
		return nil, nil
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(gxytHTML))
	if err != nil {
		return nil, err
	}

	var links []blueLink

	// Blue button (FSL) — r2.dev links
	doc.Find(`a.btn-success[href*="r2.dev"], a#fsl[href*="r2.dev"]`).Each(func(_ int, el *goquery.Selection) {
		if href, ok := el.Attr("href"); ok {
			links = append(links, blueLink{url: href, label: "FSL"})
		}
	})

	// Worker links (ddl2/ddl3 workers.dev)
	doc.Find(`a.btn-success[href*="workers.dev"], a.btn-success[href*="ddl"]`).Each(func(_ int, el *goquery.Selection) {
		if href, ok := el.Attr("href"); ok {
			links = append(links, blueLink{url: href, label: "Worker"})
		}
	})

	// Fallback: any btn-success that's not red button and not pixeldrain
	if len(links) == 0 {
		doc.Find(`a.btn-success, a.btn-success1`).Each(func(_ int, el *goquery.Selection) {
			href, _ := el.Attr("href")
			if href != "" &&
				!strings.Contains(href, "pixel.hubcloud.cx") &&
				!strings.Contains(href, "gamerxyt.com") &&
				!strings.Contains(href, "pixeldrain") {
				links = append(links, blueLink{url: href, label: "Direct"})
			}
		})
	}

	return links, nil
}

// validateBlueLink does a HEAD request to check if a blue button URL
// is reachable and appears to serve video content.
func validateBlueLink(ctx context.Context, url string) bool {
	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Referer", "https://gamerxyt.com/")

	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		return false
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "video") ||
		strings.Contains(ct, "octet-stream") ||
		strings.Contains(ct, "zip") {
		return true
	}

	// Some CDNs don't set proper content-type on HEAD, accept redirects too
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return true
	}

	return false
}

// --- Pack link resolution ---

type packLink struct {
	url   string
	label string
}

func resolvePackLinks(ctx context.Context, hubURL string) ([]packLink, error) {
	pageHTML, err := fetchPageHTML(ctx, hubURL)
	if err != nil || pageHTML == "" {
		return nil, err
	}

	gxytMatch := gamerxytRe.FindString(pageHTML)
	if gxytMatch == "" {
		return nil, nil
	}

	gxytHTML, err := fetchPageHTML(ctx, gxytMatch)
	if err != nil || gxytHTML == "" {
		return nil, nil
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(gxytHTML))
	if err != nil {
		return nil, err
	}

	var links []packLink

	// Blue button (FSL) — r2.dev links
	doc.Find(`a.btn-success[href*="r2.dev"], a#fsl[href*="r2.dev"]`).Each(func(_ int, el *goquery.Selection) {
		if href, ok := el.Attr("href"); ok {
			links = append(links, packLink{url: href, label: "FSL"})
		}
	})

	// Worker links (ddl2/ddl3 workers.dev)
	doc.Find(`a.btn-success[href*="workers.dev"], a.btn-success[href*="ddl"]`).Each(func(_ int, el *goquery.Selection) {
		if href, ok := el.Attr("href"); ok {
			links = append(links, packLink{url: href, label: "Worker"})
		}
	})

	// Pixeldrain links
	doc.Find(`a.btn-success[href*="pixeldrain"]`).Each(func(_ int, el *goquery.Selection) {
		if href, ok := el.Attr("href"); ok {
			links = append(links, packLink{url: href, label: "Pixeldrain"})
		}
	})

	// Fallback: any btn-success that's not red button
	if len(links) == 0 {
		doc.Find(`a.btn-success, a.btn-success1`).Each(func(_ int, el *goquery.Selection) {
			href, _ := el.Attr("href")
			if href != "" && !strings.Contains(href, "pixel.hubcloud.cx") && !strings.Contains(href, "gamerxyt.com") {
				links = append(links, packLink{url: href, label: "Download"})
			}
		})
	}

	return links, nil
}
