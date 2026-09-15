package db

import (
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func mkBucket(t *testing.T, d *DB, name string) *Bucket {
	t.Helper()
	b, err := d.CreateBucket(name, "")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func putKey(t *testing.T, d *DB, bucketID int64, key string) {
	t.Helper()
	if err := d.PutObjectMeta(&ObjectMeta{BucketID: bucketID, Key: key, Size: 1, ETag: "\"e\"", ContentType: "text/plain", LastModified: time.Now(), Metadata: "{}", IsLatest: true}); err != nil {
		t.Fatalf("put %q: %v", key, err)
	}
}

func putVersion(t *testing.T, d *DB, bucketID int64, key, version string, marker bool) {
	t.Helper()
	if err := d.PutObjectMetaVersioned(&ObjectMeta{BucketID: bucketID, Key: key, Size: 2, ETag: "\"v\"", ContentType: "text/plain", LastModified: time.Now(), Metadata: "{}", VersionID: version, IsLatest: true, IsDeleteMarker: marker}); err != nil {
		t.Fatalf("put %q@%q: %v", key, version, err)
	}
}

func keysOf(objs []ObjectMeta) []string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.Key
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b := mkBucket(t, d, "b1")
	putKey(t, d, b.ID, "k")
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	var n int
	if err := d2.Reader().QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(migrations) {
		t.Fatalf("schema_migrations rows = %d, want %d", n, len(migrations))
	}
	got, err := d2.GetBucket("b1")
	if err != nil || got == nil {
		t.Fatalf("bucket lost after reopen: %v", err)
	}
	if m, _ := d2.GetObjectMeta(got.ID, "k"); m == nil {
		t.Fatal("object lost after reopen")
	}
	// Meta k/v survives too.
	if err := d2.SetMeta("layout", "2"); err != nil {
		t.Fatal(err)
	}
	if v, _ := d2.GetMeta("layout"); v != "2" {
		t.Fatalf("meta = %q", v)
	}
	if v, err := d2.GetMeta("missing"); err != nil || v != "" {
		t.Fatalf("missing meta: %q %v", v, err)
	}
}

func TestMigrationsOnLegacySchema(t *testing.T) {
	// Simulate a database created before schema_migrations existed: the base
	// tables carry the columns already. Migration 1's ADD COLUMN must tolerate it.
	path := filepath.Join(t.TempDir(), "legacy.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Writer().Exec("DELETE FROM schema_migrations"); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen on legacy schema: %v", err)
	}
	d2.Close()
}

func TestListObjectsMeta_Delimiter(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "list")
	for i := 1; i <= 1500; i++ {
		putKey(t, d, b.ID, fmt.Sprintf("a/%04d", i))
	}
	putKey(t, d, b.ID, "b/x")
	putKey(t, d, b.ID, "c/y")

	objs, cps, trunc, next, err := d.ListObjectsMeta(b.ID, "", "", "/", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(cps, []string{"a/", "b/", "c/"}) {
		t.Fatalf("common prefixes %v", cps)
	}
	if len(objs) != 0 {
		t.Fatalf("objects %v", keysOf(objs))
	}
	if trunc || next != "" {
		t.Fatalf("trunc=%v next=%q", trunc, next)
	}

	// Sibling keys must not be hidden behind a >1000-key folder.
	putKey(t, d, b.ID, "b-file")
	objs, cps, trunc, _, err = d.ListObjectsMeta(b.ID, "", "", "/", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(cps, []string{"a/", "b/", "c/"}) || !equalStrings(keysOf(objs), []string{"b-file"}) || trunc {
		t.Fatalf("cps=%v objs=%v trunc=%v", cps, keysOf(objs), trunc)
	}

	// max-keys=2 → [a/, b-file] truncated, next marker "b-file"; next page b/, c/.
	objs, cps, trunc, next, err = d.ListObjectsMeta(b.ID, "", "", "/", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(cps, []string{"a/"}) || !equalStrings(keysOf(objs), []string{"b-file"}) {
		t.Fatalf("page1 cps=%v objs=%v", cps, keysOf(objs))
	}
	if !trunc || next != "b-file" {
		t.Fatalf("page1 trunc=%v next=%q", trunc, next)
	}
	objs, cps, trunc, next, err = d.ListObjectsMeta(b.ID, "", next, "/", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(cps, []string{"b/", "c/"}) || len(objs) != 0 {
		t.Fatalf("page2 cps=%v objs=%v", cps, keysOf(objs))
	}
	if trunc || next != "" {
		t.Fatalf("page2 trunc=%v next=%q", trunc, next)
	}

	// Truncation exactly at a common prefix that is the last entry.
	_, cps, trunc, next, err = d.ListObjectsMeta(b.ID, "", "b-file", "/", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(cps, []string{"b/"}) || !trunc || next != "b/" {
		t.Fatalf("cps=%v trunc=%v next=%q", cps, trunc, next)
	}
}

// TestListObjectsMeta_MarkerAtCommonPrefix_BUG documents a pagination loop:
// when a page ends on a CommonPrefix, NextMarker (v1) / the continuation
// token (v2) carry that prefix, but resuming with it re-emits the same prefix
// because the resume query is "key > marker" (matching "a/0001" > "a/")
// instead of skipping to the prefix's upper bound as the truncation probe does.
// A client following NextMarker/NextContinuationToken with a delimiter never
// terminates. Expected S3 behaviour: marker "a/" with delimiter "/" resumes
// after everything under "a/".
func TestListObjectsMeta_MarkerAtCommonPrefix_BUG(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "loop")
	for i := 0; i < 3; i++ {
		putKey(t, d, b.ID, fmt.Sprintf("a/%d", i))
		putKey(t, d, b.ID, fmt.Sprintf("b/%d", i))
	}
	putKey(t, d, b.ID, "c")

	// Page 1 ends on the common prefix "a/".
	objs, cps, trunc, next, err := d.ListObjectsMeta(b.ID, "", "", "/", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(cps, []string{"a/"}) || len(objs) != 0 || !trunc || next != "a/" {
		t.Fatalf("page1 cps=%v objs=%v trunc=%v next=%q", cps, keysOf(objs), trunc, next)
	}
	// Resuming with that marker must move past the folder.
	objs, cps, trunc, next, err = d.ListObjectsMeta(b.ID, "", next, "/", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(cps, []string{"b/"}) || len(objs) != 0 || !trunc || next != "b/" {
		t.Fatalf("BUG db/metadata.go ListObjectsMeta: marker=%q re-emits the same prefix; page2 cps=%v objs=%v trunc=%v next=%q (want cps=[b/] next=b/)", "a/", cps, keysOf(objs), trunc, next)
	}
	objs, cps, trunc, _, _ = d.ListObjectsMeta(b.ID, "", next, "/", 1)
	if len(cps) != 0 || !equalStrings(keysOf(objs), []string{"c"}) || trunc {
		t.Fatalf("page3 cps=%v objs=%v trunc=%v", cps, keysOf(objs), trunc)
	}
}

func TestListObjectsMeta_PrefixCaseAndLikeChars(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "pfx")
	putKey(t, d, b.ID, "Photos/a.jpg")
	putKey(t, d, b.ID, "photos/b.jpg")
	putKey(t, d, b.ID, "100%_done/x")
	putKey(t, d, b.ID, "100Xdone/y")
	putKey(t, d, b.ID, "100%Adone/z")

	objs, _, _, _, err := d.ListObjectsMeta(b.ID, "Photos/", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(keysOf(objs), []string{"Photos/a.jpg"}) {
		t.Fatalf("case-sensitive prefix: %v", keysOf(objs))
	}
	objs, _, _, _, _ = d.ListObjectsMeta(b.ID, "100%_", "", "", 100)
	if !equalStrings(keysOf(objs), []string{"100%_done/x"}) {
		t.Fatalf("literal %%_ prefix: %v", keysOf(objs))
	}
	objs, _, _, _, _ = d.ListObjectsMeta(b.ID, "100%", "", "", 100)
	if !equalStrings(keysOf(objs), []string{"100%Adone/z", "100%_done/x"}) {
		t.Fatalf("literal %% prefix: %v", keysOf(objs))
	}
	// Prefix + delimiter with prefix not ending at a delimiter.
	_, cps, _, _, _ := d.ListObjectsMeta(b.ID, "Pho", "", "/", 100)
	if !equalStrings(cps, []string{"Photos/"}) {
		t.Fatalf("partial prefix cps: %v", cps)
	}
	// Prefix that matches nothing.
	objs, cps, trunc, _, _ := d.ListObjectsMeta(b.ID, "zzz", "", "/", 100)
	if len(objs) != 0 || len(cps) != 0 || trunc {
		t.Fatal("expected empty result")
	}
}

func TestListObjectsMeta_MarkerPaginationAllKeysOnce(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "pages")
	var want []string
	for i := 0; i < 2500; i++ {
		k := fmt.Sprintf("k%05d-%c", (i*7919)%2500, 'a'+byte(i%26))
		want = append(want, k)
		putKey(t, d, b.ID, k)
	}
	sort.Strings(want)
	var got []string
	marker := ""
	for pages := 0; ; pages++ {
		objs, cps, trunc, next, err := d.ListObjectsMeta(b.ID, "", marker, "", 1000)
		if err != nil {
			t.Fatal(err)
		}
		if len(cps) != 0 {
			t.Fatal("no delimiter → no common prefixes")
		}
		got = append(got, keysOf(objs)...)
		if !trunc {
			if next != "" {
				t.Fatalf("next marker on last page: %q", next)
			}
			break
		}
		if next != objs[len(objs)-1].Key {
			t.Fatalf("next marker %q != last key %q", next, objs[len(objs)-1].Key)
		}
		marker = next
		if pages > 5 {
			t.Fatal("too many pages")
		}
	}
	if !equalStrings(got, want) {
		t.Fatalf("got %d keys, want %d; first diff: %s", len(got), len(want), firstDiff(got, want))
	}
}

func firstDiff(a, b []string) string {
	for i := range a {
		if i >= len(b) {
			return fmt.Sprintf("extra %q at %d", a[i], i)
		}
		if a[i] != b[i] {
			return fmt.Sprintf("index %d: %q vs %q", i, a[i], b[i])
		}
	}
	if len(b) > len(a) {
		return fmt.Sprintf("missing %q", b[len(a)])
	}
	return "none"
}

func TestListObjectsMeta_MaxKeysZero(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "zero")
	objs, _, trunc, _, err := d.ListObjectsMeta(b.ID, "", "", "", 0)
	if err != nil || len(objs) != 0 || trunc {
		t.Fatalf("empty bucket: objs=%d trunc=%v err=%v", len(objs), trunc, err)
	}
	putKey(t, d, b.ID, "x")
	objs, _, trunc, _, err = d.ListObjectsMeta(b.ID, "", "", "", 0)
	if err != nil || len(objs) != 0 || !trunc {
		t.Fatalf("non-empty bucket: objs=%d trunc=%v err=%v", len(objs), trunc, err)
	}
	// Negative is treated as zero.
	objs, _, trunc, _, _ = d.ListObjectsMeta(b.ID, "", "", "", -5)
	if len(objs) != 0 || !trunc {
		t.Fatal("negative maxKeys")
	}
}

func TestListObjectsMeta_ExcludesMarkersAndNoncurrent(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "vis")
	putVersion(t, d, b.ID, "old", "v1", false)
	putVersion(t, d, b.ID, "old", "v2", false)
	putVersion(t, d, b.ID, "gone", "v1", false)
	putVersion(t, d, b.ID, "gone", "m1", true)
	objs, _, _, _, err := d.ListObjectsMeta(b.ID, "", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Key != "old" || objs[0].VersionID != "v2" {
		t.Fatalf("got %+v", objs)
	}
	if n, _ := d.CountObjects(b.ID); n != 1 {
		t.Fatalf("count %d", n)
	}
}

func TestListObjectsMetaV2(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "v2")
	for i := 0; i < 5; i++ {
		putKey(t, d, b.ID, fmt.Sprintf("k%d", i))
	}
	if _, _, _, _, err := d.ListObjectsMetaV2(b.ID, "", "", "!!!not-base64", "", 10); !errors.Is(err, ErrInvalidContinuationToken) {
		t.Fatalf("want ErrInvalidContinuationToken, got %v", err)
	}
	objs, _, trunc, token, err := d.ListObjectsMetaV2(b.ID, "", "", "", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(keysOf(objs), []string{"k0", "k1"}) || !trunc || token == "" {
		t.Fatalf("page1 %v trunc=%v token=%q", keysOf(objs), trunc, token)
	}
	if dec, _ := base64.StdEncoding.DecodeString(token); string(dec) != "k1" {
		t.Fatalf("token decodes to %q", dec)
	}
	objs, _, trunc, token, _ = d.ListObjectsMetaV2(b.ID, "", "", token, "", 2)
	if !equalStrings(keysOf(objs), []string{"k2", "k3"}) || !trunc {
		t.Fatalf("page2 %v", keysOf(objs))
	}
	objs, _, trunc, token, _ = d.ListObjectsMetaV2(b.ID, "", "", token, "", 2)
	if !equalStrings(keysOf(objs), []string{"k4"}) || trunc || token != "" {
		t.Fatalf("page3 %v trunc=%v token=%q", keysOf(objs), trunc, token)
	}
	// start-after
	objs, _, _, _, _ = d.ListObjectsMetaV2(b.ID, "", "k2", "", "", 10)
	if !equalStrings(keysOf(objs), []string{"k3", "k4"}) {
		t.Fatalf("start-after %v", keysOf(objs))
	}
	// continuation token wins over start-after
	tok := base64.StdEncoding.EncodeToString([]byte("k3"))
	objs, _, _, _, _ = d.ListObjectsMetaV2(b.ID, "", "k0", tok, "", 10)
	if !equalStrings(keysOf(objs), []string{"k4"}) {
		t.Fatalf("token over start-after %v", keysOf(objs))
	}
}

func TestListObjectVersions(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "vers")
	putVersion(t, d, b.ID, "a", "v1", false)
	putVersion(t, d, b.ID, "a", "v2", false)
	putVersion(t, d, b.ID, "a", "v3", true)
	putVersion(t, d, b.ID, "b", "v1", false)
	putKey(t, d, b.ID, "c") // version_id ""

	all, trunc, err := d.ListObjectVersions(b.ID, "", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if trunc {
		t.Fatal("unexpected truncation")
	}
	var got []string
	for _, v := range all {
		got = append(got, v.Key+"@"+v.VersionID)
	}
	want := []string{"a@v3", "a@v2", "a@v1", "b@v1", "c@"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if !all[0].IsLatest || all[1].IsLatest || all[2].IsLatest || !all[3].IsLatest || !all[4].IsLatest {
		t.Fatal("is_latest flags wrong")
	}
	if !all[0].IsDeleteMarker {
		t.Fatal("v3 should be a marker")
	}

	// Marker pagination with page size 2.
	var paged []string
	km, vm := "", ""
	for i := 0; i < 10; i++ {
		page, more, err := d.ListObjectVersions(b.ID, "", km, vm, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range page {
			paged = append(paged, v.Key+"@"+v.VersionID)
		}
		if !more {
			break
		}
		last := page[len(page)-1]
		km, vm = last.Key, last.VersionID
		if vm == "" {
			vm = "null"
		}
	}
	if !equalStrings(paged, want) {
		t.Fatalf("paged %v want %v", paged, want)
	}
	// Key marker without version marker skips the whole key.
	page, _, _ := d.ListObjectVersions(b.ID, "", "a", "", 10)
	if len(page) != 2 || page[0].Key != "b" {
		t.Fatalf("key-marker only: %v", keysOf(page))
	}
	// Prefix filter.
	page, _, _ = d.ListObjectVersions(b.ID, "b", "", "", 10)
	if len(page) != 1 || page[0].Key != "b" {
		t.Fatalf("prefix: %v", keysOf(page))
	}
	// "null" version marker resumes after the "" row.
	page, _, _ = d.ListObjectVersions(b.ID, "", "c", "null", 10)
	if len(page) != 0 {
		t.Fatalf("after c@null: %v", keysOf(page))
	}
}

func TestGetObjectMetaByVersionNull(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "nul")
	putKey(t, d, b.ID, "k") // version ""
	m, err := d.GetObjectMetaByVersion(b.ID, "k", "null")
	if err != nil || m == nil {
		t.Fatalf("null lookup: %v %v", m, err)
	}
	if m.VersionID != "" {
		t.Fatalf("version %q", m.VersionID)
	}
	if m, _ := d.GetObjectMetaByVersion(b.ID, "k", "v9"); m != nil {
		t.Fatal("v9 should not exist")
	}
	// A literal "null" row is also matched, preferring the newest.
	putVersion(t, d, b.ID, "k", "null", false)
	m, _ = d.GetObjectMetaByVersion(b.ID, "k", "null")
	if m == nil || m.VersionID != "null" {
		t.Fatalf("expected newest null row, got %+v", m)
	}
}

func TestDeleteVersionAndPromote(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "promo")
	putVersion(t, d, b.ID, "k", "v1", false)
	putVersion(t, d, b.ID, "k", "v2", false)
	putVersion(t, d, b.ID, "k", "m3", true)

	deleted, promoted, err := d.DeleteVersionAndPromote(b.ID, "k", "m3")
	if err != nil {
		t.Fatal(err)
	}
	if deleted == nil || !deleted.IsDeleteMarker {
		t.Fatalf("deleted=%+v", deleted)
	}
	if promoted == nil || promoted.VersionID != "v2" || !promoted.IsLatest {
		t.Fatalf("promoted=%+v", promoted)
	}
	cur, _ := d.GetObjectMeta(b.ID, "k")
	if cur == nil || cur.VersionID != "v2" {
		t.Fatalf("current after promote: %+v", cur)
	}
	// Deleting a noncurrent version does not promote.
	deleted, promoted, err = d.DeleteVersionAndPromote(b.ID, "k", "v1")
	if err != nil || deleted == nil || promoted != nil {
		t.Fatalf("noncurrent delete: %+v %+v %v", deleted, promoted, err)
	}
	// Missing version → nil, nil.
	deleted, promoted, err = d.DeleteVersionAndPromote(b.ID, "k", "nope")
	if err != nil || deleted != nil || promoted != nil {
		t.Fatalf("missing: %+v %+v %v", deleted, promoted, err)
	}
	// Last version → no promotion, key gone.
	deleted, promoted, _ = d.DeleteVersionAndPromote(b.ID, "k", "v2")
	if deleted == nil || promoted != nil {
		t.Fatal("last delete")
	}
	if cur, _ := d.GetObjectMeta(b.ID, "k"); cur != nil {
		t.Fatal("key should be gone")
	}
	// "null" resolves the "" row.
	putKey(t, d, b.ID, "u")
	deleted, _, _ = d.DeleteVersionAndPromote(b.ID, "u", "null")
	if deleted == nil || deleted.VersionID != "" {
		t.Fatalf("null delete: %+v", deleted)
	}
}

func TestPutObjectMetaVersionedSingleLatest(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "latest")
	countLatest := func() int {
		var n int
		if err := d.Reader().QueryRow("SELECT COUNT(*) FROM objects WHERE bucket_id = ? AND key = 'k' AND is_latest = 1", b.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, v := range []string{"v1", "v2", "v3"} {
		putVersion(t, d, b.ID, "k", v, false)
		if n := countLatest(); n != 1 {
			t.Fatalf("after %s: %d latest rows", v, n)
		}
	}
	// Unversioned overwrite twice: one row, still latest.
	putKey(t, d, b.ID, "k")
	putKey(t, d, b.ID, "k")
	if n := countLatest(); n != 1 {
		t.Fatalf("after unversioned puts: %d latest rows", n)
	}
	var total int
	d.Reader().QueryRow("SELECT COUNT(*) FROM objects WHERE bucket_id = ? AND key = 'k'", b.ID).Scan(&total)
	if total != 4 {
		t.Fatalf("total rows %d, want 4 (v1,v2,v3,'')", total)
	}
	cur, _ := d.GetObjectMeta(b.ID, "k")
	if cur.VersionID != "" {
		t.Fatalf("current should be the unversioned row, got %q", cur.VersionID)
	}
	// Re-put of an existing version id updates in place and re-becomes latest.
	putVersion(t, d, b.ID, "k", "v2", false)
	if n := countLatest(); n != 1 {
		t.Fatalf("after re-put v2: %d latest rows", n)
	}
	if cur, _ := d.GetObjectMeta(b.ID, "k"); cur.VersionID != "v2" {
		t.Fatalf("current %q", cur.VersionID)
	}
	versions, _ := d.ListVersionsForKey(b.ID, "k")
	if len(versions) != 4 {
		t.Fatalf("ListVersionsForKey %d", len(versions))
	}
}

func TestGetExpiredObjectsCursor(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "exp")
	for i := 0; i < 5; i++ {
		putKey(t, d, b.ID, fmt.Sprintf("logs/%d", i))
	}
	putKey(t, d, b.ID, "keep/0")
	putVersion(t, d, b.ID, "logs/marker", "m", true)
	if _, err := d.Writer().Exec("UPDATE objects SET last_modified = datetime('now', '-10 days') WHERE bucket_id = ?", b.ID); err != nil {
		t.Fatal(err)
	}
	// Nothing older than 30 days.
	if objs, _ := d.GetExpiredObjects("exp", "", 30, 0, 100); len(objs) != 0 {
		t.Fatalf("30d: %v", keysOf(objs))
	}
	page1, err := d.GetExpiredObjects("exp", "logs/", 5, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(keysOf(page1), []string{"logs/0", "logs/1"}) {
		t.Fatalf("page1 %v", keysOf(page1))
	}
	page2, _ := d.GetExpiredObjects("exp", "logs/", 5, page1[len(page1)-1].ID, 100)
	if !equalStrings(keysOf(page2), []string{"logs/2", "logs/3", "logs/4"}) {
		t.Fatalf("page2 %v (marker must be excluded)", keysOf(page2))
	}
	all, _ := d.GetExpiredObjects("exp", "", 5, 0, 100)
	if len(all) != 6 {
		t.Fatalf("all prefixes: %v", keysOf(all))
	}
	// Noncurrent + orphan markers.
	putVersion(t, d, b.ID, "v", "v1", false)
	putVersion(t, d, b.ID, "v", "v2", false)
	d.Writer().Exec("UPDATE objects SET last_modified = datetime('now', '-10 days') WHERE bucket_id = ?", b.ID)
	nc, _ := d.GetExpiredNoncurrentVersions("exp", "", 5, 0, 100)
	if len(nc) != 1 || nc[0].VersionID != "v1" {
		t.Fatalf("noncurrent: %+v", nc)
	}
	orphans, _ := d.GetOrphanDeleteMarkers("exp", "", 0, 100)
	if len(orphans) != 1 || orphans[0].Key != "logs/marker" {
		t.Fatalf("orphans: %v", keysOf(orphans))
	}
	putVersion(t, d, b.ID, "logs/marker", "real", false)
	putVersion(t, d, b.ID, "logs/marker", "m2", true)
	orphans, _ = d.GetOrphanDeleteMarkers("exp", "", 0, 100)
	if len(orphans) != 0 {
		t.Fatalf("marker with sibling version reported orphan: %v", keysOf(orphans))
	}
}

func TestLifecycleRulesRoundtrip(t *testing.T) {
	d := openTestDB(t)
	mkBucket(t, d, "lc")
	rules := []LifecycleRule{
		{Name: "expire-logs", Prefix: "logs/", Status: "Enabled", ExpirationDays: 30},
		{Name: "off", Prefix: "tmp/", Status: "Disabled", NoncurrentDays: 7, AbortMultipartDays: 3, ExpireDeleteMarkers: true},
		{Name: "default-status", Prefix: "", ExpirationDays: 1},
	}
	if err := d.ReplaceLifecycleRules("lc", rules); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetLifecycleRules("lc")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rules", len(got))
	}
	byPrefix := map[string]LifecycleRule{}
	for _, r := range got {
		byPrefix[r.Prefix] = r
	}
	if r := byPrefix["logs/"]; r.Name != "expire-logs" || r.Status != "Enabled" || r.ExpirationDays != 30 || r.NoncurrentDays != 0 {
		t.Fatalf("logs/: %+v", r)
	}
	if r := byPrefix["tmp/"]; r.Status != "Disabled" || r.NoncurrentDays != 7 || r.AbortMultipartDays != 3 || !r.ExpireDeleteMarkers || r.ExpirationDays != 0 {
		t.Fatalf("tmp/: %+v", r)
	}
	if r := byPrefix[""]; r.Status != "Enabled" {
		t.Fatalf("empty status should default to Enabled: %+v", r)
	}
	// Matching picks the longest enabled expiration rule.
	m, _ := d.GetMatchingLifecycleRule("lc", "logs/a")
	if m == nil || m.Prefix != "logs/" {
		t.Fatalf("match logs/a: %+v", m)
	}
	m, _ = d.GetMatchingLifecycleRule("lc", "tmp/a")
	if m == nil || m.Prefix != "" {
		t.Fatalf("disabled rule must be skipped, got %+v", m)
	}
	// Replace shrinks.
	if err := d.ReplaceLifecycleRules("lc", []LifecycleRule{{Prefix: "x/", ExpirationDays: 2}}); err != nil {
		t.Fatal(err)
	}
	got, _ = d.GetLifecycleRules("lc")
	if len(got) != 1 || got[0].Prefix != "x/" {
		t.Fatalf("after replace: %+v", got)
	}
	all, _ := d.GetAllLifecycleRules()
	if len(all) != 1 {
		t.Fatalf("all rules: %d", len(all))
	}
	if err := d.DeleteLifecycleRules("lc"); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.GetLifecycleRules("lc"); len(got) != 0 {
		t.Fatal("rules not deleted")
	}
}

func TestReplaceWebhooks(t *testing.T) {
	d := openTestDB(t)
	mkBucket(t, d, "wh")
	if _, err := d.CreateWebhook("wh", "old", "https://old.example/hook", "", "s"); err != nil {
		t.Fatal(err)
	}
	if err := d.ReplaceWebhooks("wh", []BucketWebhook{
		{Name: "a", URL: "https://a.example/h", EventTypes: "s3:ObjectCreated:*", Secret: "sa"},
		{Name: "b", URL: "https://b.example/h"},
	}); err != nil {
		t.Fatal(err)
	}
	hooks, err := d.ListWebhooks("wh")
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 2 || hooks[0].Name != "a" || hooks[1].Name != "b" {
		t.Fatalf("hooks %+v", hooks)
	}
	if hooks[1].EventTypes != "*" || !hooks[1].Active || hooks[0].Secret != "sa" {
		t.Fatalf("defaults: %+v", hooks[1])
	}
	active, _ := d.GetActiveWebhooksForBucket("wh")
	if len(active) != 2 {
		t.Fatalf("active %d", len(active))
	}
	if err := d.ReplaceWebhooks("wh", nil); err != nil {
		t.Fatal(err)
	}
	if hooks, _ := d.ListWebhooks("wh"); len(hooks) != 0 {
		t.Fatal("replace with nil should clear")
	}
}

func TestBucketHasRowsVsObjects(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "rows")
	has, _ := d.BucketHasObjects(b.ID)
	any, _ := d.BucketHasAnyRows(b.ID)
	if has || any {
		t.Fatal("empty bucket")
	}
	putVersion(t, d, b.ID, "k", "m1", true)
	has, _ = d.BucketHasObjects(b.ID)
	any, _ = d.BucketHasAnyRows(b.ID)
	if has {
		t.Fatal("marker-only bucket must not count as having objects")
	}
	if !any {
		t.Fatal("marker row must count as a row")
	}
	if u, _ := d.GetBucketUsage(b.ID); u != 0 {
		t.Fatalf("usage %d", u)
	}
	putVersion(t, d, b.ID, "k", "v1", false)
	has, _ = d.BucketHasObjects(b.ID)
	if !has {
		t.Fatal("real version should count")
	}
	if u, _ := d.GetBucketUsage(b.ID); u != 2 {
		t.Fatalf("usage %d", u)
	}
}

func TestBucketAndCredentialLifecycle(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "creds")
	if _, err := d.CreateBucket("creds", ""); err == nil {
		t.Fatal("duplicate bucket accepted")
	}
	c, err := d.CreateCredential(b.ID, "n", "AKTEST", "secret", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Permission != "read-write" {
		t.Fatalf("default permission %q", c.Permission)
	}
	got, _ := d.GetCredentialByAccessKey("AKTEST")
	if got == nil || got.SecretKey != "secret" || got.BucketID != b.ID {
		t.Fatalf("cred %+v", got)
	}
	ids, _ := d.GetBucketIDsForAccessKey("AKTEST")
	if len(ids) != 1 || ids[0] != b.ID {
		t.Fatalf("ids %v", ids)
	}
	if err := d.SetBucketQuota("creds", 1234); err != nil {
		t.Fatal(err)
	}
	if err := d.SetBucketVersioning("creds", "Enabled"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetBucketPublicRead("creds", true); err != nil {
		t.Fatal(err)
	}
	bb, _ := d.GetBucket("creds")
	if bb.QuotaBytes != 1234 || bb.Versioning != "Enabled" || !bb.PublicRead {
		t.Fatalf("bucket %+v", bb)
	}
	if name, _ := d.GetBucketNameByID(b.ID); name != "creds" {
		t.Fatalf("name %q", name)
	}
	putKey(t, d, b.ID, "k")
	if err := d.DeleteBucket("creds"); err != nil {
		t.Fatal(err)
	}
	if c, _ := d.GetCredentialByAccessKey("AKTEST"); c != nil {
		t.Fatal("credential should cascade-delete")
	}
	if any, _ := d.BucketHasAnyRows(b.ID); any {
		t.Fatal("objects should cascade-delete")
	}
	if bb, _ := d.GetBucket("creds"); bb != nil {
		t.Fatal("bucket still present")
	}
}

func TestMultipartMetadata(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "mp")
	up := &MultipartUpload{ID: "u1", BucketID: b.ID, Key: "big", ContentType: "x/y", Metadata: "{}"}
	if err := d.CreateMultipartUpload(up); err != nil {
		t.Fatal(err)
	}
	for _, pn := range []int{3, 1, 2} {
		if err := d.PutMultipartPart(&MultipartPart{UploadID: "u1", PartNumber: pn, Size: int64(pn), ETag: fmt.Sprintf("\"%d\"", pn)}); err != nil {
			t.Fatal(err)
		}
	}
	parts, _ := d.ListMultipartParts("u1")
	if len(parts) != 3 || parts[0].PartNumber != 1 || parts[2].PartNumber != 3 {
		t.Fatalf("parts %+v", parts)
	}
	// Re-upload part 2 updates in place.
	d.PutMultipartPart(&MultipartPart{UploadID: "u1", PartNumber: 2, Size: 99, ETag: "\"new\""})
	parts, _ = d.ListMultipartParts("u1")
	if len(parts) != 3 || parts[1].Size != 99 {
		t.Fatalf("after re-put: %+v", parts)
	}
	ups, trunc, _ := d.ListMultipartUploads(b.ID, "", "", "", 10)
	if len(ups) != 1 || trunc {
		t.Fatalf("uploads %+v", ups)
	}
	stale, _ := d.ListStaleMultipartUploadsForBucket(b.ID, "", time.Hour)
	if len(stale) != 0 {
		t.Fatal("fresh upload reported stale")
	}
	d.Writer().Exec("UPDATE multipart_uploads SET created_at = datetime('now', '-2 days')")
	stale, _ = d.ListStaleMultipartUploadsForBucket(b.ID, "big", 24*time.Hour)
	if len(stale) != 1 {
		t.Fatalf("stale %d", len(stale))
	}
	stale, _ = d.ListStaleMultipartUploadsForBucket(b.ID, "other/", 24*time.Hour)
	if len(stale) != 0 {
		t.Fatal("prefix filter ignored")
	}
	ok, err := d.DeleteMultipartUpload("u1")
	if err != nil || !ok {
		t.Fatalf("delete %v %v", ok, err)
	}
	if parts, _ := d.ListMultipartParts("u1"); len(parts) != 0 {
		t.Fatal("parts should cascade")
	}
	if ok, _ := d.DeleteMultipartUpload("u1"); ok {
		t.Fatal("second delete should report false")
	}
}

func TestPrefixUpperBound(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"a", "b", true},
		{"abc", "abd", true},
		{"a\xff", "b", true},
		{"\xff\xff", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := prefixUpperBound(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("prefixUpperBound(%q) = %q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestIterateObjects(t *testing.T) {
	d := openTestDB(t)
	b := mkBucket(t, d, "iter")
	for i := 0; i < 7; i++ {
		putKey(t, d, b.ID, fmt.Sprintf("k%d", i))
	}
	var seen []string
	if err := d.IterateObjects(3, func(m ObjectMeta) error { seen = append(seen, m.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 7 {
		t.Fatalf("seen %v", seen)
	}
	stop := errors.New("stop")
	if err := d.IterateObjects(3, func(ObjectMeta) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("error not propagated: %v", err)
	}
}
