package storage

import (
	"bytes"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newFS(t *testing.T) (*FileSystem, string) {
	t.Helper()
	dir := t.TempDir()
	fs, err := NewFileSystem(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.CreateBucketDir("test-bucket"); err != nil {
		t.Fatal(err)
	}
	return fs, dir
}

func readAll(t *testing.T, rc io.ReadCloser, err error) string {
	t.Helper()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func getObj(t *testing.T, fs *FileSystem, bucket, key string) string {
	t.Helper()
	rc, err := fs.GetObject(bucket, key)
	return readAll(t, rc, err)
}

func getVer(t *testing.T, fs *FileSystem, bucket, key, ver string) string {
	t.Helper()
	rc, err := fs.GetVersionedObject(bucket, key, ver)
	return readAll(t, rc, err)
}

func md5Quoted(b []byte) string {
	sum := md5.Sum(b)
	return fmt.Sprintf("\"%x\"", sum[:])
}

func findTmpFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), tmpPrefix) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestFileSystem_PutGetRoundtrip(t *testing.T) {
	fs, _ := newFS(t)
	body := []byte("hello world")
	n, etag, err := fs.PutObject("test-bucket", "dir/hello.txt", bytes.NewReader(body), PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(body)) {
		t.Fatalf("size %d", n)
	}
	if etag != md5Quoted(body) {
		t.Fatalf("etag %q want %q", etag, md5Quoted(body))
	}
	if got := getObj(t, fs, "test-bucket", "dir/hello.txt"); got != "hello world" {
		t.Fatalf("got %q", got)
	}
	if !fs.ObjectExists("test-bucket", "dir/hello.txt") {
		t.Fatal("ObjectExists false")
	}
	if fs.ObjectExists("test-bucket", "dir/hello.txt/") {
		t.Fatal("placeholder key must not exist")
	}
	// On-disk file carries the safe extension.
	root, _ := fs.bucketRoot("test-bucket")
	if _, err := os.Stat(filepath.Join(root, "dir", "hello.txt"+safeExt)); err != nil {
		t.Fatalf("expected file with safeExt: %v", err)
	}
	if err := fs.DeleteObject("test-bucket", "dir/hello.txt"); err != nil {
		t.Fatal(err)
	}
	if fs.ObjectExists("test-bucket", "dir/hello.txt") {
		t.Fatal("still exists after delete")
	}
	// Empty parent dir cleaned up.
	if _, err := os.Stat(filepath.Join(root, "dir")); !os.IsNotExist(err) {
		t.Fatalf("parent dir should be removed, stat err=%v", err)
	}
	// Deleting a missing object is a no-op.
	if err := fs.DeleteObject("test-bucket", "nope"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.GetObject("test-bucket", "nope"); !IsNotExist(err) {
		t.Fatalf("expected not-exist, got %v", err)
	}
}

func TestFileSystem_MaxSize(t *testing.T) {
	fs, dir := newFS(t)
	_, _, err := fs.PutObject("test-bucket", "big", bytes.NewReader(make([]byte, 100)), PutOptions{MaxSize: 50})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	if fs.ObjectExists("test-bucket", "big") {
		t.Fatal("object must not exist")
	}
	if tmp := findTmpFiles(t, dir); len(tmp) != 0 {
		t.Fatalf("temp files left behind: %v", tmp)
	}
	// Exactly MaxSize is fine.
	if _, _, err := fs.PutObject("test-bucket", "ok", bytes.NewReader(make([]byte, 50)), PutOptions{MaxSize: 50}); err != nil {
		t.Fatal(err)
	}
}

func TestFileSystem_VerifyFailureKeepsPrevious(t *testing.T) {
	fs, dir := newFS(t)
	if _, _, err := fs.PutObject("test-bucket", "k", strings.NewReader("old"), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	verifyErr := errors.New("checksum bad")
	var gotSize int64
	var gotSum []byte
	_, _, err := fs.PutObject("test-bucket", "k", strings.NewReader("new!"), PutOptions{Verify: func(size int64, sum []byte) error {
		gotSize, gotSum = size, sum
		return verifyErr
	}})
	if !errors.Is(err, verifyErr) {
		t.Fatalf("want verify error, got %v", err)
	}
	if gotSize != 4 {
		t.Fatalf("verify saw size %d", gotSize)
	}
	want := md5.Sum([]byte("new!"))
	if !bytes.Equal(gotSum, want[:]) {
		t.Fatal("verify saw wrong md5")
	}
	if got := getObj(t, fs, "test-bucket", "k"); got != "old" {
		t.Fatalf("previous object clobbered: %q", got)
	}
	if tmp := findTmpFiles(t, dir); len(tmp) != 0 {
		t.Fatalf("temp files left behind: %v", tmp)
	}
}

func TestFileSystem_Versioned(t *testing.T) {
	fs, _ := newFS(t)
	if _, _, err := fs.PutVersionedObject("test-bucket", "k", "v1", strings.NewReader("one"), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fs.PutVersionedObject("test-bucket", "k", "v2", strings.NewReader("two"), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := getVer(t, fs, "test-bucket", "k", "v1"); got != "one" {
		t.Fatalf("v1 = %q", got)
	}
	if got := getVer(t, fs, "test-bucket", "k", "v2"); got != "two" {
		t.Fatalf("v2 = %q", got)
	}
	// Unversioned slot is independent.
	if _, err := fs.GetObject("test-bucket", "k"); !IsNotExist(err) {
		t.Fatalf("unversioned slot should be absent: %v", err)
	}
	if err := fs.DeleteVersionedObject("test-bucket", "k", "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.GetVersionedObject("test-bucket", "k", "v1"); !IsNotExist(err) {
		t.Fatalf("v1 should be gone: %v", err)
	}
	if got := getVer(t, fs, "test-bucket", "k", "v2"); got != "two" {
		t.Fatalf("v2 after delete = %q", got)
	}
	for _, bad := range []string{"", "../x", "a/b", ".", "..", "x\x00"} {
		if _, _, err := fs.PutVersionedObject("test-bucket", "k", bad, strings.NewReader("x"), PutOptions{}); err == nil {
			t.Errorf("version id %q accepted", bad)
		}
	}
}

const testUploadID = "0f8fad5b-d9cb-469f-a165-70867728950e"

func TestFileSystem_Multipart(t *testing.T) {
	fs, _ := newFS(t)
	p1 := bytes.Repeat([]byte("a"), 1000)
	p2 := bytes.Repeat([]byte("b"), 500)
	p3 := []byte("tail")
	for i, p := range [][]byte{p1, p2, p3} {
		n, etag, err := fs.PutMultipartPart("test-bucket", testUploadID, i+1, bytes.NewReader(p), PutOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if n != int64(len(p)) || etag != md5Quoted(p) {
			t.Fatalf("part %d: n=%d etag=%s", i+1, n, etag)
		}
	}
	// Drop part 3; assemble 1 and 2.
	if err := fs.DeleteMultipartPart("test-bucket", testUploadID, 3); err != nil {
		t.Fatal(err)
	}
	if err := fs.DeleteMultipartPart("test-bucket", testUploadID, 3); err != nil {
		t.Fatalf("second delete should be no-op: %v", err)
	}
	size, etag, err := fs.AssembleMultipartParts("test-bucket", "assembled", testUploadID, []int{1, 2}, PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if size != 1500 {
		t.Fatalf("size %d", size)
	}
	d1, d2 := md5.Sum(p1), md5.Sum(p2)
	concat := md5.Sum(append(d1[:], d2[:]...))
	want := fmt.Sprintf("\"%x-2\"", concat[:])
	if etag != want {
		t.Fatalf("etag %s want %s", etag, want)
	}
	got := getObj(t, fs, "test-bucket", "assembled")
	if got != string(p1)+string(p2) {
		t.Fatal("assembled content mismatch")
	}
	// Assembling a missing part fails and leaves no object.
	if _, _, err := fs.AssembleMultipartParts("test-bucket", "bad", testUploadID, []int{1, 3}, PutOptions{}); err == nil {
		t.Fatal("expected error for missing part")
	}
	if fs.ObjectExists("test-bucket", "bad") {
		t.Fatal("bad object must not exist")
	}
	// MaxSize applies to the assembled object too.
	if _, _, err := fs.AssembleMultipartParts("test-bucket", "capped", testUploadID, []int{1, 2}, PutOptions{MaxSize: 1000}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	if err := fs.DeleteMultipartParts("test-bucket", testUploadID); err != nil {
		t.Fatal(err)
	}
	mp, _ := fs.siblingDir("test-bucket", "multipart")
	if _, err := os.Stat(filepath.Join(mp, testUploadID)); !os.IsNotExist(err) {
		t.Fatal("upload dir should be gone")
	}
	// Invalid ids / part numbers.
	if _, _, err := fs.PutMultipartPart("test-bucket", "../etc", 1, strings.NewReader("x"), PutOptions{}); err == nil {
		t.Fatal("bad upload id accepted")
	}
	if _, _, err := fs.PutMultipartPart("test-bucket", testUploadID, 0, strings.NewReader("x"), PutOptions{}); err == nil {
		t.Fatal("part 0 accepted")
	}
	if _, _, err := fs.PutMultipartPart("test-bucket", testUploadID, 10001, strings.NewReader("x"), PutOptions{}); err == nil {
		t.Fatal("part 10001 accepted")
	}
}

func TestFileSystem_AssembleVersioned(t *testing.T) {
	fs, _ := newFS(t)
	if _, _, err := fs.PutMultipartPart("test-bucket", testUploadID, 1, strings.NewReader("abc"), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fs.AssembleMultipartPartsVersioned("test-bucket", "k", "v9", testUploadID, []int{1}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := getVer(t, fs, "test-bucket", "k", "v9"); got != "abc" {
		t.Fatalf("got %q", got)
	}
}

func TestFileSystem_DeletePrefix(t *testing.T) {
	fs, _ := newFS(t)
	if err := fs.CreateBucketDir("other-bucket"); err != nil {
		t.Fatal(err)
	}
	put := func(bucket, key string) {
		t.Helper()
		if _, _, err := fs.PutObject(bucket, key, strings.NewReader("x"), PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	put("test-bucket", "folder/a")
	put("test-bucket", "folder/sub/b")
	put("test-bucket", "folder2/c")
	put("other-bucket", "keep")

	// Non-directory prefix is a no-op.
	if err := fs.DeletePrefix("test-bucket", "folder"); err != nil {
		t.Fatal(err)
	}
	if !fs.ObjectExists("test-bucket", "folder/a") {
		t.Fatal("non-slash prefix must not delete anything")
	}
	if err := fs.DeletePrefix("test-bucket", ""); err != nil {
		t.Fatal(err)
	}

	// Escaping prefixes never touch a sibling bucket.
	for _, p := range []string{"../other-bucket/", "../../", "folder/../../other-bucket/"} {
		_ = fs.DeletePrefix("test-bucket", p)
		if !fs.ObjectExists("other-bucket", "keep") {
			t.Fatalf("prefix %q removed sibling bucket data", p)
		}
	}
	if !fs.ObjectExists("test-bucket", "folder/a") {
		t.Fatal("escape attempts must not remove in-bucket data either")
	}
	otherRoot, _ := fs.bucketRoot("other-bucket")
	if _, err := os.Stat(otherRoot); err != nil {
		t.Fatalf("sibling bucket dir removed: %v", err)
	}

	// Real folder delete.
	if err := fs.DeletePrefix("test-bucket", "folder/"); err != nil {
		t.Fatal(err)
	}
	if fs.ObjectExists("test-bucket", "folder/a") || fs.ObjectExists("test-bucket", "folder/sub/b") {
		t.Fatal("folder contents still present")
	}
	if !fs.ObjectExists("test-bucket", "folder2/c") {
		t.Fatal("sibling folder removed")
	}
	root, _ := fs.bucketRoot("test-bucket")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("bucket root removed: %v", err)
	}
}

func TestFileSystem_BucketRootRejectsBadNames(t *testing.T) {
	fs, _ := newFS(t)
	for _, name := range []string{"..", ".", "a", "../x", "Has/Slash", "UPPER", "with space", "a..b\\c", ""} {
		if _, err := fs.bucketRoot(name); err == nil {
			t.Errorf("bucket name %q accepted", name)
		}
		if _, _, err := fs.PutObject(name, "k", strings.NewReader("x"), PutOptions{}); err == nil {
			t.Errorf("PutObject with bucket %q accepted", name)
		}
		if err := fs.CreateBucketDir(name); err == nil {
			t.Errorf("CreateBucketDir(%q) accepted", name)
		}
	}
	if _, err := fs.bucketRoot("a..b"); err == nil {
		t.Error("bucket name with dots must be rejected by bucketRoot")
	}
	if _, err := fs.bucketRoot("valid-bucket-1"); err != nil {
		t.Errorf("valid name rejected: %v", err)
	}
}

func TestSafeJoin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if _, err := safeJoin(root, "a/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := safeJoin(root, "../x"); err == nil {
		t.Fatal("escape accepted")
	}
	if _, err := safeJoin(root, "a/../../x"); err == nil {
		t.Fatal("escape accepted")
	}
	if p, err := safeJoin(root, ""); err != nil || p != root {
		t.Fatalf("empty rel: %v %q", err, p)
	}
}

func TestFileSystem_LegacyMigration(t *testing.T) {
	fs, _ := newFS(t)
	root, _ := fs.bucketRoot("test-bucket")

	// "dir/" placeholder used to be stored at <root>/dir/.cloodsys3ext.
	legacyPlaceholder := filepath.Join(root, "dir") + safeExt
	if err := os.MkdirAll(filepath.Dir(legacyPlaceholder), 0700); err != nil {
		t.Fatal(err)
	}
	// Legacy raw path for "dir/" is root/dir/ + safeExt -> filepath.Join drops the
	// trailing slash, so it was root/dir.cloodsys3ext — replicate exactly what
	// LegacyObjectPath computes to seed the file.
	current, legacy := fs.LegacyObjectPath("test-bucket", "dir/", "")
	if legacy == "" {
		t.Fatal("expected a legacy path for dir/")
	}
	if current == legacy {
		t.Fatal("current and legacy must differ")
	}
	if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	moved, err := fs.MoveLegacyObject(current, legacy)
	if err != nil || !moved {
		t.Fatalf("moved=%v err=%v", moved, err)
	}
	if got := getObj(t, fs, "test-bucket", "dir/"); got != "placeholder" {
		t.Fatalf("after move: %q", got)
	}
	if _, err := os.Lstat(legacy); !os.IsNotExist(err) {
		t.Fatal("legacy file should be gone")
	}
	// Second call is a no-op.
	if moved, err := fs.MoveLegacyObject(current, legacy); err != nil || moved {
		t.Fatalf("second move: moved=%v err=%v", moved, err)
	}
	// Safe keys need no migration.
	if cur, leg := fs.LegacyObjectPath("test-bucket", "plain/key.txt", ""); leg != "" || cur == "" {
		t.Fatalf("plain key: current=%q legacy=%q", cur, leg)
	}
	// Versioned legacy path.
	cur, leg := fs.LegacyObjectPath("test-bucket", "a/../b", "v1")
	if leg == "" || !strings.HasSuffix(cur, versionSep+"v1"+safeExt) {
		t.Fatalf("versioned: current=%q legacy=%q", cur, leg)
	}
	// Escaping legacy paths are refused.
	if _, leg := fs.LegacyObjectPath("test-bucket", "../../escape", ""); leg != "" {
		t.Fatalf("escaping legacy path returned: %q", leg)
	}
}

func TestFileSystem_Variants(t *testing.T) {
	fs, _ := newFS(t)
	key := VariantCacheKey("k", "", "\"etag\"", "w10h0mfq75")
	if rc, size, err := fs.GetVariant("test-bucket", key); err != nil || rc != nil || size != 0 {
		t.Fatalf("miss should be nil,0,nil: %v %v %d", rc, err, size)
	}
	if err := fs.PutVariant("test-bucket", key, []byte("variant")); err != nil {
		t.Fatal(err)
	}
	rc, size, err := fs.GetVariant("test-bucket", key)
	if err != nil || rc == nil || size != 7 {
		t.Fatalf("hit: %v %d", err, size)
	}
	rc.Close()
	if _, _, err := fs.GetVariant("test-bucket", "../../x"); err == nil {
		t.Fatal("bad cache key accepted")
	}
	if err := fs.DeleteVariantsForKey("test-bucket", "k"); err != nil {
		t.Fatal(err)
	}
	if rc, _, _ := fs.GetVariant("test-bucket", key); rc != nil {
		rc.Close()
		t.Fatal("variant should be gone")
	}
}

func TestFileSystem_DeleteBucketDir(t *testing.T) {
	fs, _ := newFS(t)
	if _, _, err := fs.PutObject("test-bucket", "k", strings.NewReader("x"), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fs.PutMultipartPart("test-bucket", testUploadID, 1, strings.NewReader("x"), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := fs.PutVariant("test-bucket", VariantCacheKey("k", "", "e", "opt"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := fs.DeleteBucketDir("test-bucket"); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"multipart", "cache"} {
		d, _ := fs.siblingDir("test-bucket", suffix)
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("%s tree still present", suffix)
		}
	}
	root, _ := fs.bucketRoot("test-bucket")
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("bucket dir still present")
	}
}

func TestFileSystem_CustomBucketDir(t *testing.T) {
	fs, dir := newFS(t)
	custom := filepath.Join(dir, "custom")
	fs.SetBucketDir("custom-bkt", custom)
	if err := fs.CreateBucketDir("custom-bkt"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fs.PutObject("custom-bkt", "k", strings.NewReader("x"), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(custom, "custom-bkt", "k"+safeExt)); err != nil {
		t.Fatalf("object not under custom dir: %v", err)
	}
	if fs.BucketDir("custom-bkt") != custom {
		t.Fatal("BucketDir mismatch")
	}
	fs.RemoveBucketDir("custom-bkt")
	if fs.BucketDir("custom-bkt") != "" {
		t.Fatal("RemoveBucketDir did not clear")
	}
}

func TestFileSystem_GetObjectRejectsSymlink(t *testing.T) {
	fs, dir := newFS(t)
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("s"), 0600); err != nil {
		t.Fatal(err)
	}
	root, _ := fs.bucketRoot("test-bucket")
	if err := os.Symlink(secret, filepath.Join(root, "link"+safeExt)); err != nil {
		t.Skip("symlinks unsupported")
	}
	if _, err := fs.GetObject("test-bucket", "link"); err == nil {
		t.Fatal("symlink was followed")
	}
	if fs.ObjectExists("test-bucket", "link") {
		t.Fatal("symlink reported as existing object")
	}
}
