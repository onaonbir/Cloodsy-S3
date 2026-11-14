package service_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/service"
	"github.com/onaonbir/Cloodsy-S3/storage"
)

func (e *env) moveInput(src *db.ObjectMeta, dstKey string) service.CopyInput {
	return service.CopyInput{
		SrcBucket: e.bucket, Src: src, DstBucket: e.bucket, DstKey: dstKey,
		ContentType: src.ContentType, Metadata: src.Metadata,
	}
}

// A write to the source that arrives while a move is copying must wait for
// the move and must survive it (the move deletes only the versions that
// existed when it started).
func TestMove_ConcurrentWriteToSourceSurvives(t *testing.T) {
	for _, versioning := range []string{"", "Enabled"} {
		t.Run("versioning="+versioning, func(t *testing.T) {
			e := newEnv(t)
			if versioning != "" {
				e.setVersioning(versioning)
			}
			e.put("src.txt", []byte("original"), service.PutInput{})
			src := e.latest("src.txt")

			putDone := make(chan error, 1)
			restore := service.SetTestHookMoveCopied(func() {
				go func() {
					_, err := e.svc.Put(service.PutInput{
						Bucket: e.bucket, Key: "src.txt", Body: bytes.NewReader([]byte("written meanwhile")), DeclaredSize: 17,
					})
					putDone <- err
				}()
				select {
				case err := <-putDone:
					t.Errorf("a source write completed while the move held the source lock (err=%v)", err)
				case <-time.After(150 * time.Millisecond):
				}
			})
			defer restore()

			res, delErr, err := e.svc.Move(e.moveInput(src, "dst.txt"))
			if err != nil || delErr != nil || res == nil {
				t.Fatalf("move: res=%v delErr=%v err=%v", res, delErr, err)
			}
			select {
			case err := <-putDone:
				if err != nil {
					t.Fatalf("concurrent put: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("concurrent put never finished")
			}

			vs := e.versions("src.txt")
			if len(vs) != 1 || vs[0].IsDeleteMarker || !vs[0].IsLatest {
				t.Fatalf("source versions after move: %+v", vs)
			}
			if got := string(e.read(&vs[0])); got != "written meanwhile" {
				t.Fatalf("concurrent write lost: %q", got)
			}
			if got := string(e.read(e.latest("dst.txt"))); got != "original" {
				t.Fatalf("destination: %q", got)
			}
		})
	}
}

func TestMove_SourceChangedIsRefused(t *testing.T) {
	e := newEnv(t)
	e.setVersioning("Enabled")
	e.put("src.txt", []byte("v1"), service.PutInput{})
	stale := e.latest("src.txt")
	e.put("src.txt", []byte("v2"), service.PutInput{})

	_, delErr, err := e.svc.Move(e.moveInput(stale, "dst.txt"))
	if !errors.Is(err, service.ErrSourceChanged) || delErr != nil {
		t.Fatalf("err=%v delErr=%v, want ErrSourceChanged", err, delErr)
	}
	if m := e.latest("dst.txt"); m != nil {
		t.Fatalf("destination written: %+v", m)
	}
	if vs := e.versions("src.txt"); len(vs) != 2 {
		t.Fatalf("source versions touched: %+v", vs)
	}

	// Unversioned overwrite in place is detected too.
	e2 := newEnv(t)
	e2.put("a.txt", []byte("one"), service.PutInput{})
	stale = e2.latest("a.txt")
	time.Sleep(2 * time.Millisecond)
	e2.put("a.txt", []byte("two!"), service.PutInput{})
	if _, _, err := e2.svc.Move(e2.moveInput(stale, "b.txt")); !errors.Is(err, service.ErrSourceChanged) {
		t.Fatalf("unversioned: err=%v", err)
	}
	if got := string(e2.read(e2.latest("a.txt"))); got != "two!" {
		t.Fatalf("source content: %q", got)
	}
}

// Source and destination keys that hash to the same lock stripe must not
// self-deadlock.
func TestMove_SameStripeKeys(t *testing.T) {
	e := newEnv(t)
	e.put("src.txt", []byte("data"), service.PutInput{})
	dst := ""
	for i := 0; i < 100000 && dst == ""; i++ {
		k := fmt.Sprintf("k%d", i)
		if service.StripeForTest(e.bucket.ID, k) == service.StripeForTest(e.bucket.ID, "src.txt") {
			dst = k
		}
	}
	if dst == "" {
		t.Fatal("no colliding key found")
	}
	done := make(chan error, 1)
	go func() {
		_, delErr, err := e.svc.Move(e.moveInput(e.latest("src.txt"), dst))
		if err == nil {
			err = delErr
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("move deadlocked on a shared stripe")
	}
	if e.latest("src.txt") != nil || e.latest(dst) == nil {
		t.Fatal("move did not move")
	}
}

// --- multipart part/abort race ---------------------------------------------

func (e *env) newUpload(key string) string {
	e.t.Helper()
	id := uuid.New().String()
	if err := e.db.CreateMultipartUpload(&db.MultipartUpload{ID: id, BucketID: e.bucket.ID, Key: key, ContentType: "application/octet-stream", Metadata: "{}"}); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestPutPart_WaitsForAbortThenFails(t *testing.T) {
	e := newEnv(t)
	id := e.newUpload("big.bin")

	// Simulate an abort in progress: hold the exclusive upload lock.
	unlock := e.svc.LockUpload(id)
	errc := make(chan error, 1)
	go func() {
		_, err := e.svc.PutPart(e.bucket, id, 1, bytes.NewReader([]byte("part")), storage.PutOptions{})
		errc <- err
	}()
	select {
	case err := <-errc:
		t.Fatalf("part upload ran during abort: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := e.db.DeleteMultipartUpload(id); err != nil {
		t.Fatal(err)
	}
	e.store.DeleteMultipartParts(bucketName, id)
	unlock()

	select {
	case err := <-errc:
		if !errors.Is(err, service.ErrNoSuchUpload) {
			t.Fatalf("part after abort: %v, want ErrNoSuchUpload", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("part upload never resumed")
	}
	if _, err := os.Stat(filepath.Join(e.root, "."+bucketName+"-multipart", id)); !os.IsNotExist(err) {
		t.Fatalf("part staged after abort: %v", err)
	}
	if err := e.svc.AbortMultipart(e.bucket, id); !errors.Is(err, service.ErrNoSuchUpload) {
		t.Fatalf("second abort: %v", err)
	}
	if n := e.svc.UploadLockCountForTest(); n != 0 {
		t.Fatalf("upload locks leaked: %d", n)
	}
}

func TestPutPart_WrongBucket(t *testing.T) {
	e := newEnv(t)
	id := e.newUpload("x")
	other, err := e.db.CreateBucket("other-bucket", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PutPart(other, id, 1, bytes.NewReader([]byte("p")), storage.PutOptions{}); !errors.Is(err, service.ErrNoSuchUpload) {
		t.Fatalf("err=%v", err)
	}
	if _, err := e.svc.PutPart(e.bucket, id, 1, bytes.NewReader([]byte("p")), storage.PutOptions{}); err != nil {
		t.Fatalf("own bucket: %v", err)
	}
}

func TestUploadLocks_PartsParallelUploadsIndependent(t *testing.T) {
	e := newEnv(t)
	r1 := e.svc.RLockUploadForTest("a")

	// A second part upload of the same upload is not blocked.
	got := make(chan struct{})
	go func() {
		e.svc.RLockUploadForTest("a")()
		close(got)
	}()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("parallel part uploads serialized")
	}

	// Complete/abort of that upload waits for the part.
	wdone := make(chan struct{})
	go func() {
		e.svc.LockUpload("a")()
		close(wdone)
	}()
	select {
	case <-wdone:
		t.Fatal("abort did not wait for an in-flight part")
	case <-time.After(100 * time.Millisecond):
	}

	// Another upload is unaffected.
	other := make(chan struct{})
	go func() {
		e.svc.LockUpload("b")()
		close(other)
	}()
	select {
	case <-other:
	case <-time.After(5 * time.Second):
		t.Fatal("unrelated upload blocked")
	}

	r1()
	select {
	case <-wdone:
	case <-time.After(5 * time.Second):
		t.Fatal("abort never acquired the lock")
	}
	if n := e.svc.UploadLockCountForTest(); n != 0 {
		t.Fatalf("upload locks leaked: %d", n)
	}
}
