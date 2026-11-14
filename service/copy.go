package service

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/onaonbir/Cloodsy-S3/db"
)

// ErrSourceChanged reports that the source of a move was overwritten or
// deleted between the caller's lookup and the move itself.
var ErrSourceChanged = errors.New("source changed during move")

// CopyInput describes a server-side copy of one object version.
type CopyInput struct {
	SrcBucket *db.Bucket
	Src       *db.ObjectMeta // must not be a delete marker
	DstBucket *db.Bucket
	DstKey    string
	// ContentType and Metadata are the destination's final values (the
	// source's for MetadataDirective=COPY, the request's for REPLACE).
	ContentType string
	Metadata    string
	MaxSize     int64 // 0 = MaxPutObjectSize
}

func (in CopyInput) putInput(body io.Reader) PutInput {
	maxSize := in.MaxSize
	if maxSize <= 0 {
		maxSize = MaxPutObjectSize
	}
	return PutInput{
		Bucket:       in.DstBucket,
		Key:          in.DstKey,
		Body:         body,
		ContentType:  in.ContentType,
		Metadata:     in.Metadata,
		DeclaredSize: in.Src.Size,
		MaxSize:      maxSize,
		EventType:    "s3:ObjectCreated:Copy",
	}
}

// Copy streams a stored object into a new object through Put, so quota,
// versioning, image optimization and the s3:ObjectCreated:Copy webhook event
// behave exactly as for an S3 CopyObject request.
func (s *Objects) Copy(in CopyInput) (*PutResult, error) {
	if in.Src == nil || in.Src.IsDeleteMarker {
		return nil, ErrNoSuchKey
	}
	reader, err := s.Open(in.SrcBucket, in.Src)
	if err != nil {
		return nil, fmt.Errorf("open source object: %w", err)
	}
	defer reader.Close()
	return s.Put(in.putInput(reader))
}

// testHookMoveCopied, when set (tests only), runs after a move has copied the
// source and before it deletes it.
var testHookMoveCopied func()

// Move copies in.Src like Copy and then permanently removes the source key's
// versions. Source and destination key locks are held for the whole
// operation, so no other write to either key can interleave. The source must
// still have in.Src as its latest version once the locks are held
// (ErrSourceChanged otherwise), and only the version rows that existed when
// the copy started are deleted. deleteErr reports a failed source delete
// after a successful copy.
func (s *Objects) Move(in CopyInput) (res *PutResult, deleteErr, err error) {
	if in.Src == nil || in.Src.IsDeleteMarker {
		return nil, nil, ErrNoSuchKey
	}
	if err := s.precheckPut(in.putInput(nil)); err != nil {
		return nil, nil, err
	}
	sb, key := in.SrcBucket, in.Src.Key
	unlock := s.locks.lockPair(sb.ID, key, in.DstBucket.ID, in.DstKey)
	defer unlock()

	versions, err := s.DB.ListVersionsForKey(sb.ID, key)
	if err != nil {
		return nil, nil, err
	}
	var cur *db.ObjectMeta
	for i := range versions {
		if versions[i].IsLatest {
			cur = &versions[i]
			break
		}
	}
	if cur == nil || cur.IsDeleteMarker || cur.ID != in.Src.ID || cur.VersionID != in.Src.VersionID ||
		cur.ETag != in.Src.ETag || !cur.LastModified.Equal(in.Src.LastModified) {
		return nil, nil, ErrSourceChanged
	}

	reader, err := s.Open(sb, cur)
	if err != nil {
		return nil, nil, fmt.Errorf("open source object: %w", err)
	}
	res, err = s.putLocked(in.putInput(reader))
	reader.Close()
	if err != nil {
		return nil, nil, err
	}
	if testHookMoveCopied != nil {
		testHookMoveCopied()
	}

	ids := make([]int64, len(versions))
	for i := range versions {
		ids[i] = versions[i].ID
	}
	if _, err := s.DB.DeleteObjectRows(sb.ID, key, ids); err != nil {
		return res, err, nil
	}
	for _, v := range versions {
		if !v.IsDeleteMarker {
			s.removeBytes(sb.Name, key, v.VersionID)
		}
	}
	s.Store.DeleteVariantsForKey(sb.Name, key)
	s.emit(sb.Name, "s3:ObjectRemoved:Delete", key, 0, "", "", time.Now().UTC())
	return res, nil, nil
}
