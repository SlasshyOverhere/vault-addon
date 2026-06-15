package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

const (
	repoOwner = "SlasshyOverhere"
	repoName  = "vault-addon"
)

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func checkForUpdates() {
	log.Printf("[update] checking for updates...")

	client := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequest("GET", fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName), nil)
	req.Header.Set("User-Agent", "vault-addon/"+currentVersion)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[update] failed to check: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("[update] github returned %d", resp.StatusCode)
		return
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		log.Printf("[update] failed to parse response: %v", err)
		return
	}

	remoteVersion := strings.TrimPrefix(release.TagName, "v")
	if remoteVersion == "" {
		return
	}

	// Find the right asset for this platform
	assetName := fmt.Sprintf("vault-addon-%s-%s%s", runtime.GOOS, runtime.GOARCH, exeSuffix())

	var downloadURL string
	for _, a := range release.Assets {
		if a.Name == assetName {
			downloadURL = a.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		log.Printf("[update] no asset found for %s/%s", runtime.GOOS, runtime.GOARCH)
		return
	}

	// Download the remote binary
	remoteBinary, err := downloadFile(client, downloadURL)
	if err != nil {
		log.Printf("[update] failed to download: %v", err)
		return
	}

	remoteHash := sha256Hex(remoteBinary)

	// Read current binary
	selfPath, err := os.Executable()
	if err != nil {
		log.Printf("[update] can't find own path: %v", err)
		return
	}
	selfData, err := os.ReadFile(selfPath)
	if err != nil {
		log.Printf("[update] can't read self: %v", err)
		return
	}
	selfHash := sha256Hex(selfData)

	if remoteVersion == currentVersion && remoteHash == selfHash {
		log.Printf("[update] already up to date (v%s)", currentVersion)
		return
	}

	if remoteVersion != currentVersion {
		log.Printf("[update] new version available: v%s -> v%s", currentVersion, remoteVersion)
	} else {
		log.Printf("[update] same version but different binary, updating...")
	}

	// Replace current binary
	if err := os.WriteFile(selfPath, remoteBinary, 0755); err != nil {
		// Try writing to temp and replacing
		tmpPath := selfPath + ".tmp"
		if err2 := os.WriteFile(tmpPath, remoteBinary, 0755); err2 != nil {
			log.Printf("[update] failed to write: %v", err)
			return
		}
		log.Printf("[update] wrote to %s — restart to apply", tmpPath)
		return
	}

	log.Printf("[update] updated to v%s — restart to apply", remoteVersion)
}

func downloadFile(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
