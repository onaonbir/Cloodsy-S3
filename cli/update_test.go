package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTarGz(t *testing.T, dir, name string, entries map[string][]byte) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, data := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeZip(t *testing.T, dir, name string, entries map[string][]byte) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, data := range entries {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestParseChecksums(t *testing.T) {
	data := []byte("abc")
	sum := sha256Hex(data)
	manifest := sum + "  cloodsys3-linux-amd64.tar.gz\n" +
		strings.ToUpper(sum) + " *cloodsys3-windows-amd64.zip\n" +
		"\n# comment\n" +
		sum + "  dist/cloodsys3-darwin-arm64.tar.gz\n"
	sums, err := parseChecksums(strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, name := range []string{"cloodsys3-linux-amd64.tar.gz", "cloodsys3-windows-amd64.zip", "cloodsys3-darwin-arm64.tar.gz"} {
		if got := sums[name]; got != sum {
			t.Errorf("%s: got %q want %q", name, got, sum)
		}
	}

	bad := []string{
		"",
		"notahash  file.tar.gz\n",
		"deadbeef  file.tar.gz\n",
		"onlyonefield\n",
	}
	for _, m := range bad {
		if _, err := parseChecksums(strings.NewReader(m)); err == nil {
			t.Errorf("expected error for manifest %q", m)
		}
	}
}

func TestVerifySHA256(t *testing.T) {
	data := []byte("hello world")
	if err := verifySHA256(data, sha256Hex(data)); err != nil {
		t.Fatalf("valid digest rejected: %v", err)
	}
	if err := verifySHA256(data, strings.ToUpper(sha256Hex(data))); err != nil {
		t.Fatalf("upper-case digest rejected: %v", err)
	}
	if err := verifySHA256(append(data, '!'), sha256Hex(data)); err == nil {
		t.Fatal("tampered data accepted")
	}
	if err := verifySHA256(data, ""); err == nil {
		t.Fatal("empty digest accepted")
	}
}

func TestExtractFromTarGz(t *testing.T) {
	dir := t.TempDir()
	want := []byte("\x7fELF-binary-bytes")
	p := writeTarGz(t, dir, "ok.tar.gz", map[string][]byte{
		"README.md":        []byte("docs"),
		"LICENSE":          []byte("license"),
		"./cloodsys3":      want,
		"cloodsys3.sha256": []byte("nope"),
	})
	raw, _ := os.ReadFile(p)
	got, err := extractFromTarGz(bytes.NewReader(raw), []string{"cloodsys3"}, maxBinaryBytes)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}

	// Legacy platform-suffixed name is also accepted when listed.
	p = writeTarGz(t, dir, "legacy.tar.gz", map[string][]byte{"cloodsys3-linux-amd64": want})
	raw, _ = os.ReadFile(p)
	if _, err := extractFromTarGz(bytes.NewReader(raw), []string{"cloodsys3", "cloodsys3-linux-amd64"}, maxBinaryBytes); err != nil {
		t.Fatalf("legacy name: %v", err)
	}

	// An archive whose first regular file is not the binary must not be used.
	p = writeTarGz(t, dir, "wrong.tar.gz", map[string][]byte{"evil": []byte("x"), "other": []byte("y")})
	raw, _ = os.ReadFile(p)
	if _, err := extractFromTarGz(bytes.NewReader(raw), []string{"cloodsys3"}, maxBinaryBytes); err == nil {
		t.Fatal("archive without the named entry was accepted")
	}

	// Size bound is enforced.
	big := bytes.Repeat([]byte("A"), 4096)
	p = writeTarGz(t, dir, "big.tar.gz", map[string][]byte{"cloodsys3": big})
	raw, _ = os.ReadFile(p)
	if _, err := extractFromTarGz(bytes.NewReader(raw), []string{"cloodsys3"}, 1024); err == nil {
		t.Fatal("oversized entry accepted")
	}

	// Garbage input.
	if _, err := extractFromTarGz(bytes.NewReader([]byte("not gzip")), []string{"cloodsys3"}, maxBinaryBytes); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestExtractFromZip(t *testing.T) {
	dir := t.TempDir()
	want := []byte("MZ-pe-bytes")
	p := writeZip(t, dir, "ok.zip", map[string][]byte{
		"LICENSE":       []byte("license"),
		"cloodsys3.exe": want,
	})
	raw, _ := os.ReadFile(p)
	got, err := extractFromZip(raw, []string{"cloodsys3.exe"}, maxBinaryBytes)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}

	// Raw zip bytes must never be mistaken for the executable.
	if bytes.Equal(got, raw) {
		t.Fatal("returned raw zip bytes")
	}

	p = writeZip(t, dir, "missing.zip", map[string][]byte{"other.exe": want})
	raw, _ = os.ReadFile(p)
	if _, err := extractFromZip(raw, []string{"cloodsys3.exe"}, maxBinaryBytes); err == nil {
		t.Fatal("zip without the named entry was accepted")
	}

	big := bytes.Repeat([]byte("A"), 4096)
	p = writeZip(t, dir, "big.zip", map[string][]byte{"cloodsys3.exe": big})
	raw, _ = os.ReadFile(p)
	if _, err := extractFromZip(raw, []string{"cloodsys3.exe"}, 1024); err == nil {
		t.Fatal("oversized entry accepted")
	}

	if _, err := extractFromZip([]byte("not a zip"), []string{"cloodsys3.exe"}, maxBinaryBytes); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestCheckExecutableMagic(t *testing.T) {
	cases := []struct {
		goos string
		data []byte
		ok   bool
	}{
		{"linux", []byte("\x7fELF...."), true},
		{"linux", []byte("MZ......"), false},
		{"windows", []byte("MZ......"), true},
		{"windows", []byte("\x7fELF...."), false},
		{"darwin", []byte{0xCF, 0xFA, 0xED, 0xFE, 0, 0}, true},
		{"darwin", []byte{0xCA, 0xFE, 0xBA, 0xBE, 0, 0}, true},
		{"darwin", []byte("<html>"), false},
		{"linux", []byte("ab"), false},
	}
	for _, c := range cases {
		err := checkExecutableMagic(c.data, c.goos)
		if (err == nil) != c.ok {
			t.Errorf("%s %q: ok=%v err=%v", c.goos, c.data, c.ok, err)
		}
	}
}

func TestInstallBinary(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "cloodsys3")
	if err := os.WriteFile(exe, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := installBinary(exe, []byte("new")); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "new" {
		t.Fatalf("binary not replaced: %q", got)
	}
	if _, err := os.Stat(exe + ".new"); err == nil {
		t.Fatal(".new left behind")
	}
	if _, err := os.Stat(exe + ".old"); err == nil {
		t.Fatal(".old left behind on success")
	}
	info, _ := os.Stat(exe)
	if info.Mode()&0111 == 0 {
		t.Fatal("installed binary is not executable")
	}

	// Unwritable directory: nothing changes.
	if os.Getuid() != 0 {
		ro := filepath.Join(dir, "ro")
		os.Mkdir(ro, 0755)
		roExe := filepath.Join(ro, "cloodsys3")
		os.WriteFile(roExe, []byte("old"), 0755)
		os.Chmod(ro, 0555)
		defer os.Chmod(ro, 0755)
		if err := installBinary(roExe, []byte("new")); err == nil {
			t.Fatal("expected failure in read-only dir")
		}
		got, _ = os.ReadFile(roExe)
		if string(got) != "old" {
			t.Fatalf("binary changed despite failure: %q", got)
		}
	}
}

func TestIsDevVersion(t *testing.T) {
	for _, v := range []string{"", "dev", "vdev", "unknown", " DEV "} {
		if !isDevVersion(v) {
			t.Errorf("%q should be dev", v)
		}
	}
	for _, v := range []string{"1.2.3", "v1.2.3", "0.0.1"} {
		if isDevVersion(v) {
			t.Errorf("%q should not be dev", v)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"v1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.2.0", "1.10.0", -1},
		{"2.0.0", "1.99.99", 1},
		{"1.1", "1.1.0", 0},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestAcceptedEntryNames(t *testing.T) {
	names := acceptedEntryNames(getAssetName())
	if len(names) != 2 {
		t.Fatalf("want 2 names, got %v", names)
	}
	if !strings.HasPrefix(names[0], binaryName) || !strings.HasPrefix(names[1], binaryName+"-") {
		t.Fatalf("unexpected names %v", names)
	}
}

func TestPasswordPolicy(t *testing.T) {
	if err := validatePassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
	if err := validatePassword(strings.Repeat("x", 73)); err == nil {
		t.Fatal("73-byte password accepted")
	}
	if err := validatePassword(strings.Repeat("x", 72)); err != nil {
		t.Fatalf("72-byte password rejected: %v", err)
	}
	pw, err := generatePassword()
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) != generatedPasswordLen {
		t.Fatalf("generated password length %d", len(pw))
	}
	if err := validatePassword(pw); err != nil {
		t.Fatalf("generated password fails policy: %v", err)
	}
	if _, _, err := resolvePassword("secret123", true); err == nil {
		t.Fatal("--password and --generate together accepted")
	}
}
