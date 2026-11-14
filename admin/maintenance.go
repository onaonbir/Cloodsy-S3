package admin

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/httpx"
	"github.com/onaonbir/Cloodsy-S3/storage"
)

// Maintenance scan limits: each reported list is capped, and so is the total
// work (database rows checked and files visited), so a scan of a huge store
// returns in bounded time with truncated=true.
const (
	scanListCap        = 1000
	scanMaxObjects     = 1_000_000
	scanMaxFiles       = 1_000_000
	maxOrphanDeletions = 1000
)

type orphanDir struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

type missingFile struct {
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
}

type untrackedFile struct {
	Bucket string `json:"bucket"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
}

type scanBudget struct {
	objects, files int
	truncated      bool
}

func (b *scanBudget) takeFile() bool {
	if b.files >= scanMaxFiles {
		b.truncated = true
		return false
	}
	b.files++
	return true
}

// tryStartScan claims the single maintenance slot (scan or orphan delete).
// Scans walk whole storage trees, so they never run concurrently.
func (h *Handler) tryStartScan(w http.ResponseWriter) bool {
	if !h.scanBusy.CompareAndSwap(false, true) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a scan is already running"})
		return false
	}
	return true
}

func (h *Handler) endScan() { h.scanBusy.Store(false) }

func absClean(p string) string {
	if p == "" {
		return ""
	}
	if a, err := filepath.Abs(p); err == nil {
		return filepath.Clean(a)
	}
	return filepath.Clean(p)
}

// foldPathCase is true on platforms whose default filesystems compare names
// case-insensitively, where "/Data" and "/data" are the same directory.
var foldPathCase = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

func pathEqual(a, b string) bool {
	if foldPathCase {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// isWithin reports whether p equals dir or lies below it (both are cleaned;
// case-insensitive where the filesystem is).
func isWithin(p, dir string) bool {
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	if pathEqual(p, dir) {
		return true
	}
	d := strings.TrimSuffix(dir, string(os.PathSeparator)) + string(os.PathSeparator)
	return len(p) > len(d) && pathEqual(p[:len(d)], d)
}

// pathForms returns the cleaned absolute path plus its symlink-resolved form
// (when it differs and resolves), so protections hold through symlinked
// configuration paths.
func pathForms(p string) []string {
	p = absClean(p)
	if p == "" {
		return nil
	}
	out := []string{p}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		if r = absClean(r); r != p {
			out = append(out, r)
		}
	}
	return out
}

// orphanKind tells what a candidate directory claims to be, which decides the
// content it must have to be accepted as an orphan.
type orphanKind int

const (
	orphanBucketDir orphanKind = iota
	orphanMultipartDir
	orphanCacheDir
)

type orphanCandidate struct {
	path string
	kind orphanKind
}

// scanGuards holds the paths an orphan candidate may never equal, contain or
// (for live bucket trees) lie inside.
type scanGuards struct {
	// noContain: a candidate that is, or is an ancestor of, any of these is
	// refused (storage roots, bucket trees, database, ACME cache, config and
	// TLS file directories).
	noContain []string
	// noEnter: a candidate inside any of these is refused (live bucket trees
	// and their hidden siblings).
	noEnter []string
}

func (g *scanGuards) protect(p string) { g.noContain = append(g.noContain, pathForms(p)...) }

func (g *scanGuards) protectTree(p string) {
	forms := pathForms(p)
	g.noContain = append(g.noContain, forms...)
	g.noEnter = append(g.noEnter, forms...)
}

func (g *scanGuards) allows(candidate string) bool {
	for _, c := range pathForms(candidate) {
		for _, p := range g.noContain {
			if isWithin(p, c) {
				return false
			}
		}
		for _, p := range g.noEnter {
			if isWithin(c, p) {
				return false
			}
		}
	}
	return true
}

// storageRoots returns the default root and every distinct custom storage
// directory, plus the guards a scan must respect.
func (h *Handler) storageRoots(buckets []db.Bucket) (roots []string, guards *scanGuards) {
	guards = &scanGuards{}
	add := func(p string) {
		p = absClean(p)
		if p == "" {
			return
		}
		for _, r := range roots {
			if pathEqual(r, p) {
				return
			}
		}
		roots = append(roots, p)
	}
	add(h.Storage.RootDir)
	for _, b := range buckets {
		if b.StorageDir != "" {
			add(b.StorageDir)
		}
	}
	for _, r := range roots {
		guards.protect(r)
	}
	for _, b := range buckets {
		if b.StorageDir != "" {
			guards.protect(b.StorageDir)
		}
		if p, err := h.Storage.BucketPath(b.Name); err == nil {
			guards.protectTree(p)
		}
		for _, base := range []string{h.Storage.RootDir, b.StorageDir, h.Storage.BucketDir(b.Name)} {
			if base == "" {
				continue
			}
			guards.protectTree(filepath.Join(base, b.Name))
			for _, suf := range storage.SiblingSuffixes {
				guards.protectTree(filepath.Join(base, "."+b.Name+"-"+suf))
			}
		}
	}
	if c := h.Config; c != nil {
		if p := c.Database.Path; p != "" {
			guards.protect(filepath.Dir(absClean(p)))
		}
		acmeDir := c.Server.TLS.ACME.CacheDir
		if acmeDir == "" && c.Database.Path != "" {
			acmeDir = filepath.Join(filepath.Dir(c.Database.Path), "acme")
		}
		if acmeDir != "" {
			guards.protect(acmeDir)
		}
		if c.Path != "" {
			guards.protect(filepath.Dir(absClean(c.Path)))
		}
		for _, f := range []string{
			c.Server.TLS.CertFile, c.Server.TLS.KeyFile,
			c.Admin.TLS.CertFile, c.Admin.TLS.KeyFile,
			c.WebDAV.TLS.CertFile, c.WebDAV.TLS.KeyFile,
		} {
			if f != "" {
				guards.protect(filepath.Dir(absClean(f)))
			}
		}
	}
	return roots, guards
}

// orphanCandidates lists directories directly under the storage roots whose
// name looks like bucket data (a bucket-named dir, or a ".<bucket>-multipart"
// / ".<bucket>-cache" sibling) for a bucket that is not in the database and
// that no guard protects. The content is not checked here; see verifyOrphan.
func (h *Handler) orphanCandidates(buckets []db.Bucket) []orphanCandidate {
	names := make(map[string]bool, len(buckets))
	for _, b := range buckets {
		names[b.Name] = true
	}
	roots, guards := h.storageRoots(buckets)
	var out []orphanCandidate
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Type() != fs.ModeDir {
				continue // regular files, symlinks, devices, ...
			}
			name := e.Name()
			bucketName := ""
			kind := orphanBucketDir
			switch {
			case storage.IsValidBucketDirName(name):
				bucketName = name
			case strings.HasPrefix(name, "."):
				for _, suf := range storage.SiblingSuffixes {
					if mid, ok := strings.CutSuffix(name[1:], "-"+suf); ok && storage.IsValidBucketDirName(mid) {
						bucketName = mid
						switch suf {
						case "multipart":
							kind = orphanMultipartDir
						case "cache":
							kind = orphanCacheDir
						default:
							bucketName = "" // a sibling kind we cannot verify
						}
						break
					}
				}
			}
			if bucketName == "" || names[bucketName] {
				continue
			}
			p := filepath.Join(root, name)
			if guards.allows(p) {
				out = append(out, orphanCandidate{path: p, kind: kind})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

var (
	uploadDirName  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	partFileName   = regexp.MustCompile(`^[0-9]{1,5}\.cloodsys3ext$`)
	cacheDirName   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	cacheFileName  = regexp.MustCompile(`^[0-9a-f]{64}\.cloodsys3ext$`)
	stagedTempName = regexp.MustCompile(`^\.tmp-[0-9]+$`) // os.CreateTemp(".tmp-*")
)

// verifyOrphan walks a candidate (bounded by budget and ctx) and reports its
// size and whether it provably is a Cloodsy bucket tree: only directories
// and the files this server writes there, no symlinks or special files.
// Anything unexpected, an unreadable entry or an exhausted budget means "not
// an orphan" - a missed orphan is harmless, deleting foreign data is not.
func verifyOrphan(ctx context.Context, c orphanCandidate, budget *scanBudget) (int64, bool) {
	fi, err := os.Lstat(c.path)
	if err != nil || !fi.IsDir() {
		return 0, false
	}
	var total int64
	ok := true
	werr := filepath.WalkDir(c.path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			ok = false
			return filepath.SkipAll
		}
		if p == c.path {
			return nil
		}
		if !budget.takeFile() {
			ok = false
			return filepath.SkipAll
		}
		rel, rerr := filepath.Rel(c.path, p)
		if rerr != nil {
			ok = false
			return filepath.SkipAll
		}
		depth := strings.Count(rel, string(os.PathSeparator)) + 1
		name := d.Name()
		isDir := d.Type() == fs.ModeDir
		isFile := d.Type() == 0 // regular file
		if !isDir && !isFile {
			ok = false // symlink, socket, device, pipe, ...
			return filepath.SkipAll
		}
		accepted := false
		switch c.kind {
		case orphanBucketDir:
			accepted = isDir || storage.IsObjectDataFile(name)
		case orphanMultipartDir:
			accepted = (depth == 1 && isDir && uploadDirName.MatchString(name)) ||
				(depth == 2 && isFile && (partFileName.MatchString(name) || stagedTempName.MatchString(name)))
		case orphanCacheDir:
			accepted = (depth == 1 && isDir && cacheDirName.MatchString(name)) ||
				(depth == 2 && isFile && (cacheFileName.MatchString(name) || stagedTempName.MatchString(name)))
		}
		if !accepted {
			ok = false
			return filepath.SkipAll
		}
		if isFile {
			info, ierr := d.Info()
			if ierr != nil || !info.Mode().IsRegular() {
				ok = false
				return filepath.SkipAll
			}
			total += info.Size()
		}
		return nil
	})
	if werr != nil {
		return 0, false
	}
	return total, ok
}

// handleMaintenanceScan runs a read-only consistency check between the
// database and the storage directories.
// POST /admin/maintenance/scan
func (h *Handler) handleMaintenanceScan(w http.ResponseWriter, r *http.Request) {
	if !h.tryStartScan(w) {
		return
	}
	defer h.endScan()
	ctx := r.Context()
	buckets, err := h.DB.ListBuckets()
	if err != nil {
		h.Logger.Error("maintenance scan: list buckets", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	clearDeadlines(w)
	budget := &scanBudget{}

	orphans := []orphanDir{}
	for _, c := range h.orphanCandidates(buckets) {
		if ctx.Err() != nil {
			return // client went away
		}
		if len(orphans) >= scanListCap {
			budget.truncated = true
			break
		}
		if size, ok := verifyOrphan(ctx, c, budget); ok {
			orphans = append(orphans, orphanDir{Path: c.path, Bytes: size})
		}
	}

	missing := []missingFile{}
	untracked := []untrackedFile{}
	for _, b := range buckets {
		if ctx.Err() != nil {
			return
		}
		if budget.objects >= scanMaxObjects {
			budget.truncated = true
			break
		}
		expected := map[string]struct{}{}
		complete := true
		err := h.DB.IterateBucketObjects(b.ID, 1000, func(m db.ObjectMeta) bool {
			if ctx.Err() != nil {
				complete = false
				return false
			}
			if budget.objects >= scanMaxObjects {
				budget.truncated = true
				complete = false
				return false
			}
			budget.objects++
			if m.IsDeleteMarker {
				return true
			}
			p, perr := h.Storage.ObjectPath(b.Name, m.Key, m.VersionID)
			if perr != nil {
				return true
			}
			expected[p] = struct{}{}
			fi, serr := os.Lstat(p)
			if (serr != nil && os.IsNotExist(serr)) || (serr == nil && !fi.Mode().IsRegular()) {
				if len(missing) < scanListCap {
					missing = append(missing, missingFile{Bucket: b.Name, Key: m.Key, VersionID: apiVersionID(m.VersionID)})
				} else {
					budget.truncated = true
				}
			}
			return true
		})
		if err != nil {
			h.Logger.Error("maintenance scan: iterate objects", "bucket", b.Name, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if ctx.Err() != nil {
			return
		}
		if !complete {
			break // the expected set is partial: an untracked check would lie
		}
		root, err := h.Storage.BucketPath(b.Name)
		if err != nil {
			continue
		}
		filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
			if ctx.Err() != nil {
				return filepath.SkipAll
			}
			if werr != nil || d.IsDir() {
				return nil
			}
			if !budget.takeFile() {
				return filepath.SkipAll
			}
			if !d.Type().IsRegular() || !storage.IsObjectDataFile(d.Name()) {
				return nil
			}
			if _, ok := expected[p]; ok {
				return nil
			}
			if len(untracked) >= scanListCap {
				budget.truncated = true
				return filepath.SkipAll
			}
			var size int64
			if fi, err := d.Info(); err == nil {
				size = fi.Size()
			}
			untracked = append(untracked, untrackedFile{Bucket: b.Name, Path: p, Bytes: size})
			return nil
		})
	}
	if ctx.Err() != nil {
		return
	}

	h.Logger.Info("maintenance scan finished", "orphan_dirs", len(orphans), "missing_files", len(missing), "untracked_files", len(untracked), "scanned_objects", budget.objects, "truncated", budget.truncated)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"orphan_dirs":     orphans,
		"missing_files":   missing,
		"untracked_files": untracked,
		"scanned_objects": budget.objects,
		"truncated":       budget.truncated,
	})
}

// handleDeleteOrphanDirs removes directories a fresh scan reports as orphan
// bucket directories. Client paths are only matched against that server-side
// list; anything else rejects the whole request. Every target's content is
// re-verified right before it is removed.
// POST /admin/maintenance/orphan-dirs/delete
func (h *Handler) handleDeleteOrphanDirs(w http.ResponseWriter, r *http.Request, actor string) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}
	if len(req.Paths) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "paths required"})
		return
	}
	if len(req.Paths) > maxOrphanDeletions {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too many paths"})
		return
	}
	if !h.tryStartScan(w) {
		return
	}
	defer h.endScan()
	ctx := r.Context()
	buckets, err := h.DB.ListBuckets()
	if err != nil {
		h.Logger.Error("orphan delete: list buckets", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	allowed := map[string]orphanCandidate{}
	for _, c := range h.orphanCandidates(buckets) {
		allowed[c.path] = c
	}
	clearDeadlines(w)
	budget := &scanBudget{}
	targets := make([]orphanCandidate, 0, len(req.Paths))
	seen := map[string]bool{}
	for _, p := range req.Paths {
		// Exact match against the server's own list: no cleaning or
		// resolving of client input beyond that.
		c, ok := allowed[p]
		if ok && !seen[p] {
			_, ok = verifyOrphan(ctx, c, budget)
		}
		if ctx.Err() != nil {
			return
		}
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not an orphan directory: " + httpx.SanitizeLog(p)})
			return
		}
		if !seen[p] {
			seen[p] = true
			targets = append(targets, c)
		}
	}

	deleted := 0
	failed := []map[string]string{}
	for _, c := range targets {
		if ctx.Err() != nil {
			return
		}
		p := c.path
		// A bucket may have been created with this name since the scan.
		name := filepath.Base(p)
		if strings.HasPrefix(name, ".") {
			for _, suf := range storage.SiblingSuffixes {
				if mid, ok := strings.CutSuffix(name[1:], "-"+suf); ok {
					name = mid
					break
				}
			}
		}
		if b, err := h.DB.GetBucket(name); err != nil || b != nil {
			failed = append(failed, map[string]string{"path": p, "error": "bucket exists"})
			continue
		}
		fi, err := os.Lstat(p)
		if err != nil || !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
			failed = append(failed, map[string]string{"path": p, "error": "not a directory"})
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			failed = append(failed, map[string]string{"path": p, "error": err.Error()})
			continue
		}
		deleted++
		h.Logger.Warn("orphan directory deleted via admin API", "path", httpx.SanitizeLog(p), "by", httpx.SanitizeLog(actor))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": deleted, "failed": failed})
}
