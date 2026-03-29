package cli

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

const githubRepo = "onaonbir/Cloodsy-S3"

type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func fetchLatestRelease() (*githubRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to check for updates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("failed to parse release info: %w", err)
	}
	return &release, nil
}

func getAssetName() string {
	os := runtime.GOOS   // linux, darwin, windows
	arch := runtime.GOARCH // amd64, arm64, arm

	if arch == "arm" {
		arch = "armv7"
	}

	name := fmt.Sprintf("cloodsys3-%s-%s", os, arch)
	if runtime.GOOS == "windows" {
		return name + ".zip"
	}
	return name + ".tar.gz"
}

func compareVersions(current, latest string) int {
	// Remove v prefix
	current = strings.TrimPrefix(current, "v")
	latest = strings.TrimPrefix(latest, "v")

	cParts := strings.Split(current, ".")
	lParts := strings.Split(latest, ".")

	for i := 0; i < 3; i++ {
		var c, l int
		if i < len(cParts) {
			fmt.Sscanf(cParts[i], "%d", &c)
		}
		if i < len(lParts) {
			fmt.Sscanf(lParts[i], "%d", &l)
		}
		if c < l {
			return -1
		}
		if c > l {
			return 1
		}
	}
	return 0
}

func RunUpdateCheck(currentVersion string) error {
	fmt.Printf("Current version: %s\n", currentVersion)
	fmt.Printf("Checking for updates...\n")

	release, err := fetchLatestRelease()
	if err != nil {
		return err
	}

	latestVersion := strings.TrimPrefix(release.TagName, "v")
	fmt.Printf("Latest version:  %s\n", latestVersion)

	if compareVersions(currentVersion, latestVersion) >= 0 {
		fmt.Printf("\nYou are up to date!\n")
		return nil
	}

	fmt.Printf("\nUpdate available! Run: cloodsys3 update\n")
	fmt.Printf("Release: %s\n", release.HTMLURL)
	return nil
}

func RunUpdate(currentVersion string) error {
	fmt.Printf("Current version: %s\n", currentVersion)
	fmt.Printf("Checking for updates...\n\n")

	release, err := fetchLatestRelease()
	if err != nil {
		return err
	}

	latestVersion := strings.TrimPrefix(release.TagName, "v")

	if compareVersions(currentVersion, latestVersion) >= 0 {
		fmt.Printf("Latest version:  %s\n", latestVersion)
		fmt.Printf("\nAlready up to date!\n")
		return nil
	}

	fmt.Printf("New version:     %s\n\n", latestVersion)

	// Find the right asset
	assetName := getAssetName()
	var downloadURL string
	for _, asset := range release.Assets {
		if asset.Name == assetName {
			downloadURL = asset.BrowserDownloadURL
			break
		}
	}

	if downloadURL == "" {
		return fmt.Errorf("no binary found for %s/%s (expected: %s)\nDownload manually: %s",
			runtime.GOOS, runtime.GOARCH, assetName, release.HTMLURL)
	}

	// Download
	fmt.Printf("Downloading %s...\n", assetName)
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	// Get current executable path
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}

	// Extract binary from tar.gz
	var newBinary []byte
	if strings.HasSuffix(assetName, ".tar.gz") {
		newBinary, err = extractFromTarGz(resp.Body)
	} else {
		// For windows zip, read the whole thing (simplified)
		newBinary, err = io.ReadAll(resp.Body)
	}
	if err != nil {
		return fmt.Errorf("extract failed: %w", err)
	}

	fmt.Printf("Downloaded %d bytes.\n", len(newBinary))

	// Replace binary
	// 1. Rename current binary to .old
	oldPath := execPath + ".old"
	os.Remove(oldPath) // remove any previous .old
	if err := os.Rename(execPath, oldPath); err != nil {
		return fmt.Errorf("cannot rename current binary: %w (try running with sudo)", err)
	}

	// 2. Write new binary
	if err := os.WriteFile(execPath, newBinary, 0755); err != nil {
		// Rollback
		os.Rename(oldPath, execPath)
		return fmt.Errorf("cannot write new binary: %w", err)
	}

	// 3. Remove old
	os.Remove(oldPath)

	fmt.Printf("\nUpdated to v%s!\n", latestVersion)
	fmt.Printf("Restart the server to apply the update.\n")
	return nil
}

func extractFromTarGz(r io.Reader) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		// The binary is the only file in the archive
		if header.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			return data, nil
		}
	}
	return nil, fmt.Errorf("no file found in archive")
}

// CheckUpdateInBackground checks for updates and logs if available.
// Non-blocking — meant to be called with go keyword.
func CheckUpdateInBackground(currentVersion string, logFn func(string, ...any)) {
	release, err := fetchLatestRelease()
	if err != nil {
		return // silently ignore
	}
	latestVersion := strings.TrimPrefix(release.TagName, "v")
	if compareVersions(currentVersion, latestVersion) < 0 {
		logFn("New version available: v%s (current: v%s). Run 'cloodsys3 update' to upgrade.", latestVersion, currentVersion)
	}
}
