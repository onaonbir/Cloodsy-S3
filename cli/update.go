package cli

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	githubRepo    = "onaonbir/Cloodsy-S3"
	binaryName    = "cloodsys3"
	checksumsName = "checksums.txt"

	// maxArchiveBytes bounds the downloaded release archive.
	maxArchiveBytes int64 = 200 << 20
	// maxBinaryBytes bounds the decompressed executable (zip-bomb guard).
	maxBinaryBytes int64 = 200 << 20
	// maxChecksumsBytes bounds checksums.txt.
	maxChecksumsBytes int64 = 1 << 20
)

type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// isDevVersion reports whether the running binary carries no real release
// version (local build). Such builds are never updated automatically.
func isDevVersion(v string) bool {
	v = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(v), "v"))
	return v == "" || v == "dev" || v == "unknown"
}

func userAgent(version string) string {
	return fmt.Sprintf("%s-updater/%s (%s; %s)", binaryName, strings.TrimPrefix(version, "v"), runtime.GOOS, runtime.GOARCH)
}

func newRequest(url, version, accept string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent(version))
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return req, nil
}

func fetchLatestRelease(version string) (*githubRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	req, err := newRequest(url, version, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to check for updates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxChecksumsBytes)).Decode(&release); err != nil {
		return nil, fmt.Errorf("failed to parse release info: %w", err)
	}
	return &release, nil
}

// downloadBytes fetches url into memory, refusing bodies larger than limit.
func downloadBytes(url, version string, limit int64) ([]byte, error) {
	req, err := newRequest(url, version, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("download too large (%d bytes, limit %d)", resp.ContentLength, limit)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download too large (limit %d bytes)", limit)
	}
	return data, nil
}

func getAssetName() string {
	os := runtime.GOOS     // linux, darwin, windows
	arch := runtime.GOARCH // amd64, arm64, arm

	if arch == "arm" {
		arch = "armv7"
	}

	name := fmt.Sprintf("%s-%s-%s", binaryName, os, arch)
	if runtime.GOOS == "windows" {
		return name + ".zip"
	}
	return name + ".tar.gz"
}

// acceptedEntryNames lists the archive entries that may be installed as the
// new executable: the canonical name and the platform-suffixed legacy name.
func acceptedEntryNames(assetName string) []string {
	platform := strings.TrimSuffix(strings.TrimSuffix(assetName, ".tar.gz"), ".zip")
	if runtime.GOOS == "windows" {
		return []string{binaryName + ".exe", platform + ".exe"}
	}
	return []string{binaryName, platform}
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
	if isDevVersion(currentVersion) {
		fmt.Println("This is a dev build; update checks are disabled.")
		return nil
	}
	fmt.Printf("Checking for updates...\n")

	release, err := fetchLatestRelease(currentVersion)
	if err != nil {
		return err
	}

	latestVersion := strings.TrimPrefix(release.TagName, "v")
	fmt.Printf("Latest version:  %s\n", latestVersion)

	if compareVersions(currentVersion, latestVersion) >= 0 {
		fmt.Printf("\nYou are up to date!\n")
		return nil
	}

	fmt.Printf("\nUpdate available! Run: %s update\n", binaryName)
	fmt.Printf("Release: %s\n", release.HTMLURL)
	return nil
}

func RunUpdate(currentVersion string) error {
	fmt.Printf("Current version: %s\n", currentVersion)
	if isDevVersion(currentVersion) {
		fmt.Println("This is a dev build, not updating. Build from source or install a release binary.")
		return nil
	}
	fmt.Printf("Checking for updates...\n\n")

	release, err := fetchLatestRelease(currentVersion)
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

	// Find the right asset and the checksum manifest.
	assetName := getAssetName()
	var downloadURL, checksumsURL string
	for _, asset := range release.Assets {
		switch asset.Name {
		case assetName:
			downloadURL = asset.BrowserDownloadURL
		case checksumsName:
			checksumsURL = asset.BrowserDownloadURL
		}
	}

	if downloadURL == "" {
		return fmt.Errorf("no binary found for %s/%s (expected: %s)\nDownload manually: %s",
			runtime.GOOS, runtime.GOARCH, assetName, release.HTMLURL)
	}
	if checksumsURL == "" {
		return fmt.Errorf("release %s has no %s; refusing to install an unverified binary\nDownload manually: %s",
			release.TagName, checksumsName, release.HTMLURL)
	}

	// Checksums first, so a bad download is rejected before anything else happens.
	fmt.Printf("Downloading %s...\n", checksumsName)
	sums, err := downloadBytes(checksumsURL, currentVersion, maxChecksumsBytes)
	if err != nil {
		return fmt.Errorf("%s: %w", checksumsName, err)
	}
	checksums, err := parseChecksums(bytes.NewReader(sums))
	if err != nil {
		return fmt.Errorf("%s: %w", checksumsName, err)
	}
	expected, ok := checksums[assetName]
	if !ok {
		return fmt.Errorf("%s has no entry for %s; refusing to install an unverified binary", checksumsName, assetName)
	}

	fmt.Printf("Downloading %s...\n", assetName)
	archive, err := downloadBytes(downloadURL, currentVersion, maxArchiveBytes)
	if err != nil {
		return err
	}
	if err := verifySHA256(archive, expected); err != nil {
		return fmt.Errorf("%s: %w — the download is corrupt or tampered with, nothing was installed", assetName, err)
	}
	fmt.Printf("Downloaded %d bytes, SHA-256 verified.\n", len(archive))

	// Extract the executable.
	var newBinary []byte
	if strings.HasSuffix(assetName, ".zip") {
		newBinary, err = extractFromZip(archive, acceptedEntryNames(assetName), maxBinaryBytes)
	} else {
		newBinary, err = extractFromTarGz(bytes.NewReader(archive), acceptedEntryNames(assetName), maxBinaryBytes)
	}
	if err != nil {
		return fmt.Errorf("extract failed: %w", err)
	}
	if err := checkExecutableMagic(newBinary, runtime.GOOS); err != nil {
		return fmt.Errorf("extract failed: %w", err)
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(execPath); err == nil {
		execPath = resolved
	}

	if err := installBinary(execPath, newBinary); err != nil {
		return err
	}

	fmt.Printf("\nUpdated to v%s!\n", latestVersion)
	fmt.Printf("Restart the server to apply the update.\n")
	return nil
}

// installBinary atomically replaces execPath with data. The new file is fully
// written next to the target first; the running executable is then renamed
// to .old (Windows forbids overwriting a running .exe but allows renaming it)
// and the new file is moved into place. If anything fails the previous
// binary is restored and the .old copy is kept.
func installBinary(execPath string, data []byte) error {
	dir := filepath.Dir(execPath)
	newPath := execPath + ".new"
	oldPath := execPath + ".old"

	os.Remove(newPath)
	f, err := os.OpenFile(newPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w (try running with sudo / as administrator)", dir, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(newPath)
		return fmt.Errorf("cannot write new binary: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(newPath)
		return fmt.Errorf("cannot flush new binary: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("cannot write new binary: %w", err)
	}
	if err := os.Chmod(newPath, 0755); err != nil && runtime.GOOS != "windows" {
		os.Remove(newPath)
		return fmt.Errorf("cannot chmod new binary: %w", err)
	}

	os.Remove(oldPath) // stale copy from a previous update, may fail on Windows if still locked
	if err := os.Rename(execPath, oldPath); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("cannot rename current binary: %w (try running with sudo / as administrator)", err)
	}
	if err := os.Rename(newPath, execPath); err != nil {
		// Roll back: put the previous binary back in place, keep nothing new.
		if rbErr := os.Rename(oldPath, execPath); rbErr != nil {
			return fmt.Errorf("cannot install new binary: %w; rollback also failed: %v — previous binary is at %s", err, rbErr, oldPath)
		}
		os.Remove(newPath)
		return fmt.Errorf("cannot install new binary: %w (previous binary restored)", err)
	}

	if err := os.Remove(oldPath); err != nil && runtime.GOOS == "windows" {
		fmt.Printf("Note: previous binary left at %s (locked while running); delete it after restarting.\n", oldPath)
	}
	return nil
}

// parseChecksums reads a sha256sum-style manifest ("<hex>  <name>" per line)
// and returns a name -> lowercase hex digest map.
func parseChecksums(r io.Reader) (map[string]string, error) {
	sums := make(map[string]string)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("malformed line %q", line)
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != sha256.Size*2 {
			return nil, fmt.Errorf("malformed digest in line %q", line)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return nil, fmt.Errorf("malformed digest in line %q", line)
		}
		// sha256sum writes "*name" for binary mode; strip any directory too.
		name := path.Base(strings.TrimPrefix(fields[len(fields)-1], "*"))
		sums[name] = sum
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(sums) == 0 {
		return nil, errors.New("empty checksum manifest")
	}
	return sums, nil
}

// verifySHA256 compares data against an expected hex digest.
func verifySHA256(data []byte, expectedHex string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, strings.TrimSpace(expectedHex)) {
		return fmt.Errorf("SHA-256 mismatch: expected %s, got %s", strings.ToLower(expectedHex), got)
	}
	return nil
}

func nameAccepted(entry string, wanted []string) bool {
	base := path.Base(strings.ReplaceAll(entry, "\\", "/"))
	for _, w := range wanted {
		if base == w {
			return true
		}
	}
	return false
}

// readBounded reads at most limit bytes and fails if the stream is longer.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("entry exceeds %d bytes", limit)
	}
	if len(data) == 0 {
		return nil, errors.New("entry is empty")
	}
	return data, nil
}

// extractFromTarGz returns the contents of the single regular file whose base
// name is in wanted. Any other entry is ignored; decompression is bounded.
func extractFromTarGz(r io.Reader, wanted []string, limit int64) ([]byte, error) {
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
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if !nameAccepted(header.Name, wanted) {
			continue
		}
		if header.Size > limit {
			return nil, fmt.Errorf("entry %s exceeds %d bytes", header.Name, limit)
		}
		return readBounded(tr, limit)
	}
	return nil, fmt.Errorf("no entry named %s found in archive", strings.Join(wanted, " or "))
}

// extractFromZip returns the contents of the zip entry whose base name is in
// wanted. Only the executable is read; decompression is bounded.
func extractFromZip(data []byte, wanted []string, limit int64) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !nameAccepted(f.Name, wanted) {
			continue
		}
		if int64(f.UncompressedSize64) > limit {
			return nil, fmt.Errorf("entry %s exceeds %d bytes", f.Name, limit)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return readBounded(rc, limit)
	}
	return nil, fmt.Errorf("no entry named %s found in archive", strings.Join(wanted, " or "))
}

// checkExecutableMagic rejects payloads that are clearly not a native
// executable for the running platform (e.g. an HTML error page inside a
// well-formed archive).
func checkExecutableMagic(data []byte, goos string) error {
	if len(data) < 4 {
		return errors.New("extracted file is too small to be an executable")
	}
	switch goos {
	case "windows":
		if data[0] == 'M' && data[1] == 'Z' {
			return nil
		}
	case "darwin":
		switch {
		case bytes.Equal(data[:4], []byte{0xCF, 0xFA, 0xED, 0xFE}),
			bytes.Equal(data[:4], []byte{0xCE, 0xFA, 0xED, 0xFE}),
			bytes.Equal(data[:4], []byte{0xCA, 0xFE, 0xBA, 0xBE}):
			return nil
		}
	default:
		if bytes.Equal(data[:4], []byte{0x7F, 'E', 'L', 'F'}) {
			return nil
		}
	}
	return fmt.Errorf("extracted file is not a %s executable", goos)
}

// CheckUpdateInBackground checks for updates and logs if available.
// Non-blocking — meant to be called with go keyword. Dev builds never check.
func CheckUpdateInBackground(currentVersion string, logFn func(string, ...any)) {
	if isDevVersion(currentVersion) {
		return
	}
	release, err := fetchLatestRelease(currentVersion)
	if err != nil {
		return // silently ignore
	}
	latestVersion := strings.TrimPrefix(release.TagName, "v")
	if compareVersions(currentVersion, latestVersion) < 0 {
		logFn("New version available: v%s (current: v%s). Run '%s update' to upgrade.", latestVersion, strings.TrimPrefix(currentVersion, "v"), binaryName)
	}
}
