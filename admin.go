package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// --- Admin API handlers ---

func handleAPIListSites(w http.ResponseWriter, r *http.Request) {
	sites := getSites()
	if sites == nil {
		sites = []Site{}
	}
	writeJSON(w, map[string]any{"sites": sites})
}

func handleAPIAddSite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	// Validate the site before adding
	if err := validateSite(r.Context(), req.URL); err != nil {
		msg := err.Error()
		status := http.StatusUnprocessableEntity
		if strings.Contains(msg, "unreachable") {
			status = http.StatusBadGateway
		} else if strings.Contains(msg, "required") || strings.Contains(msg, "invalid") || strings.Contains(msg, "must") {
			status = http.StatusBadRequest
		} else if strings.Contains(msg, "already registered") {
			status = http.StatusConflict
		}
		writeJSONStatus(w, status, map[string]any{"error": msg})
		return
	}

	site, err := addSite(req.URL)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "already registered") {
			status = http.StatusConflict
		}
		writeJSONStatus(w, status, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, map[string]any{"ok": true, "site": site})
	invalidateCacheForSite()
}

func handleAPIRemoveSite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	if err := removeSite(req.URL); err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSONStatus(w, status, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, map[string]any{"ok": true})
	invalidateCacheForSite()
}

// --- Validation ---

func validateSite(ctx context.Context, rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return fmt.Errorf("URL is required")
	}

	// Add scheme if missing
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = "https://" + rawURL
	}

	// Parse and normalize
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL must use http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("URL must have a host")
	}
	normalized := fmt.Sprintf("%s://%s", u.Scheme, strings.ToLower(u.Host))
	normalized = strings.TrimSuffix(normalized, "/")

	// Reachability + template check: hit /?s=test
	searchURL := normalized + "/?s=test"
	resp, err := httpGet(ctx, searchURL, nil)
	if err != nil {
		return fmt.Errorf("site unreachable: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("site unreachable: HTTP %d", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to parse response: %v", err)
	}

	// Check for .movie-card elements (the template's search result container)
	if doc.Find(".movie-card").Length() > 0 {
		return nil // Supported!
	}

	// Retry with a more common search term
	searchURL2 := normalized + "/?s=the"
	resp2, err := httpGet(ctx, searchURL2, nil)
	if err != nil {
		return fmt.Errorf("site does not use the supported template")
	}
	defer resp2.Body.Close()

	doc2, err := goquery.NewDocumentFromReader(resp2.Body)
	if err != nil {
		return fmt.Errorf("site does not use the supported template")
	}

	if doc2.Find(".movie-card").Length() > 0 {
		return nil // Supported!
	}

	return fmt.Errorf("site does not use the supported template (expected .movie-card elements)")
}

// --- Admin UI ---

func handleAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, adminHTML)
}

// writeJSONStatus writes a JSON response with a specific status code.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

const adminHTML = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>vault-addon Admin</title>
<style>
body{font-family:system-ui;max-width:900px;margin:2rem auto;padding:0 1rem;line-height:1.6;color:#eee;background:#111}
h1{color:#0cf}h2{color:#fa0;border-bottom:1px solid #333;padding-bottom:.3rem}
code{background:#222;padding:.15rem .4rem;border-radius:3px;font-size:.9em}
table{border-collapse:collapse;width:100%}th,td{border:1px solid #333;padding:.4rem .6rem;text-align:left}th{background:#1a1a1a}
a{color:#0cf}a:hover{text-decoration:underline}
.btn{display:inline-block;padding:.5rem 1rem;border:none;border-radius:4px;cursor:pointer;font-size:.95rem;font-weight:600;transition:opacity .15s}
.btn:hover{opacity:.85}
.btn-primary{background:#0cf;color:#111}
.btn-danger{background:#e53;color:#fff}
.btn-sm{padding:.25rem .6rem;font-size:.85rem}
input[type=text]{background:#1a1a1a;border:1px solid #444;color:#eee;padding:.5rem .75rem;border-radius:4px;font-size:1rem;width:100%;box-sizing:border-box}
input[type=text]:focus{outline:none;border-color:#0cf}
.form-row{display:flex;gap:.75rem;align-items:stretch}
.form-row input[type=text]{flex:1}
#status{margin-top:.5rem;padding:.5rem .75rem;border-radius:4px;display:none;font-size:.9rem}
#status.ok{display:block;background:#1a3a1a;border:1px solid #0f0;color:#0f0}
#status.err{display:block;background:#3a1a1a;border:1px solid #e53;color:#fa0}
.site-enabled{color:#0f0}.site-disabled{color:#888}
.header-row{display:flex;justify-content:space-between;align-items:center}
.header-row a{font-size:.9rem;font-weight:normal}
</style></head><body>
<div class="header-row">
<h1>vault-addon Admin</h1>
<a href="/">&#8592; Back to Docs</a>
</div>

<h2>Add Site</h2>
<p style="color:#888;font-size:.9rem">Enter a site URL. The server will check if it uses the supported template before adding it.</p>
<div class="form-row">
<input type="text" id="site-url" placeholder="https://example.com" />
<button class="btn btn-primary" onclick="addSite()">Validate &amp; Add</button>
</div>
<div id="status"></div>

<h2 style="margin-top:2rem">Registered Sites</h2>
<table>
<thead><tr><th>Site</th><th>URL</th><th>Status</th><th>Added</th><th>Action</th></tr></thead>
<tbody id="sites-body"><tr><td colspan="5" style="color:#888">Loading...</td></tr></tbody>
</table>

<script>
const statusEl = document.getElementById('status');

function showStatus(msg, ok) {
  statusEl.textContent = msg;
  statusEl.className = ok ? 'ok' : 'err';
}

async function loadSites() {
  try {
    const res = await fetch('/api/sites');
    const data = await res.json();
    const tbody = document.getElementById('sites-body');
    if (!data.sites || data.sites.length === 0) {
      tbody.innerHTML = '<tr><td colspan="5" style="color:#888">No sites registered. Add one above.</td></tr>';
      return;
    }
    tbody.innerHTML = data.sites.map(s => {
      const statusClass = s.enabled ? 'site-enabled' : 'site-disabled';
      const statusText = s.enabled ? 'Active' : 'Disabled';
      const added = s.addedAt ? new Date(s.addedAt).toLocaleDateString() : '-';
      return '<tr>'
        + '<td>' + esc(s.name) + '</td>'
        + '<td><code>' + esc(s.url) + '</code></td>'
        + '<td class="' + statusClass + '">' + statusText + '</td>'
        + '<td>' + added + '</td>'
        + '<td><button class="btn btn-danger btn-sm" onclick="removeSite(\'' + esc(s.url) + '\')">Remove</button></td>'
        + '</tr>';
    }).join('');
  } catch(e) {
    document.getElementById('sites-body').innerHTML = '<tr><td colspan="5" style="color:#e53">Failed to load sites</td></tr>';
  }
}

async function addSite() {
  const input = document.getElementById('site-url');
  const url = input.value.trim();
  if (!url) { showStatus('Please enter a URL', false); return; }

  showStatus('Validating...', true);
  statusEl.className = 'ok';

  try {
    const res = await fetch('/api/sites', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({url})
    });
    const data = await res.json();
    if (res.ok) {
      showStatus('Site added: ' + data.site.name, true);
      input.value = '';
      loadSites();
    } else {
      showStatus(data.error || 'Failed to add site', false);
    }
  } catch(e) {
    showStatus('Network error: ' + e.message, false);
  }
}

async function removeSite(url) {
  if (!confirm('Remove ' + url + '?')) return;
  try {
    const res = await fetch('/api/sites/remove', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({url})
    });
    const data = await res.json();
    if (res.ok) {
      loadSites();
    } else {
      showStatus(data.error || 'Failed to remove site', false);
    }
  } catch(e) {
    showStatus('Network error: ' + e.message, false);
  }
}

function esc(s) {
  const d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}

// Enter key submits
document.getElementById('site-url').addEventListener('keydown', e => {
  if (e.key === 'Enter') addSite();
});

loadSites();
</script>
</body></html>`
