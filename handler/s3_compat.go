package handler

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/s3xml"
	"github.com/onaonbir/Cloodsy-S3/storage"
)

var defaultOwner = s3xml.Owner{ID: "cloodsys3", DisplayName: "cloodsys3"}

// --- 1. GetBucketLocation ---

func (h *Handler) GetBucketLocation(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	result := s3xml.LocationResult{
		Xmlns:              "http://s3.amazonaws.com/doc/2006-03-01/",
		LocationConstraint: h.Config.Server.Region,
	}
	h.writeXML(w, http.StatusOK, result)
}

// --- 2. ListMultipartUploads ---

func (h *Handler) ListMultipartUploads(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	query := r.URL.Query()
	prefix := query.Get("prefix")
	keyMarker := query.Get("key-marker")
	uploadIDMarker := query.Get("upload-id-marker")
	encodingType := query.Get("encoding-type")
	maxUploads, ok := parseMaxKeys(query.Get("max-uploads"))
	if !ok {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}

	uploads, truncated, err := h.DB.ListMultipartUploads(bucket.ID, prefix, keyMarker, uploadIDMarker, maxUploads)
	if err != nil {
		h.Logger.Error("list multipart uploads error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	result := s3xml.ListMultipartUploadsResult{
		Xmlns:          "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:         bucketName,
		KeyMarker:      keyMarker,
		UploadIdMarker: uploadIDMarker,
		MaxUploads:     maxUploads,
		IsTruncated:    truncated,
		Prefix:         EncodeKeyIfNeeded(prefix, encodingType),
		Delimiter:      query.Get("delimiter"),
	}
	if encodingType == "url" {
		result.EncodingType = "url"
	}

	for _, u := range uploads {
		result.Uploads = append(result.Uploads, s3xml.MultipartUploadEntry{
			Key:          EncodeKeyIfNeeded(u.Key, encodingType),
			UploadId:     u.ID,
			Initiator:    defaultOwner,
			Owner:        defaultOwner,
			Initiated:    u.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			StorageClass: "STANDARD",
		})
	}

	if truncated && len(uploads) > 0 {
		last := uploads[len(uploads)-1]
		result.NextKeyMarker = EncodeKeyIfNeeded(last.Key, encodingType)
		result.NextUploadIdMarker = last.ID
	}

	h.writeXML(w, http.StatusOK, result)
}

// --- 3. ListParts ---

func (h *Handler) ListParts(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, key := getBucketAndKey(r)
	bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	uploadID := r.URL.Query().Get("uploadId")
	upload, err := h.DB.GetMultipartUpload(uploadID)
	if err != nil || upload == nil || upload.BucketID != bucket.ID {
		s3err.WriteError(w, r, s3err.ErrNoSuchUpload)
		return
	}

	parts, err := h.DB.ListMultipartParts(uploadID)
	if err != nil {
		h.Logger.Error("list parts error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	maxParts, ok := parseMaxKeys(r.URL.Query().Get("max-parts"))
	if !ok {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}
	marker := 0
	if v := r.URL.Query().Get("part-number-marker"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			marker = n
		}
	}
	filtered := parts[:0:0]
	for _, p := range parts {
		if p.PartNumber > marker {
			filtered = append(filtered, p)
		}
	}
	truncated := len(filtered) > maxParts
	if truncated {
		filtered = filtered[:maxParts]
	}

	result := s3xml.ListPartsResult{
		Xmlns:            "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:           bucketName,
		Key:              key,
		UploadId:         uploadID,
		Initiator:        defaultOwner,
		Owner:            defaultOwner,
		StorageClass:     "STANDARD",
		PartNumberMarker: marker,
		MaxParts:         maxParts,
		IsTruncated:      truncated,
	}
	if truncated && len(filtered) > 0 {
		result.NextPartNumberMarker = filtered[len(filtered)-1].PartNumber
	}
	for _, p := range filtered {
		result.Parts = append(result.Parts, s3xml.PartEntry{
			PartNumber:   p.PartNumber,
			LastModified: p.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         p.ETag,
			Size:         p.Size,
		})
	}

	h.writeXML(w, http.StatusOK, result)
}

// --- 4. GetBucketAcl / GetObjectAcl (stubs) ---

func (h *Handler) GetBucketAcl(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	h.writeACL(w)
}

func (h *Handler) GetObjectAcl(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, key := getBucketAndKey(r)
	bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	if h.lookupObject(w, r, bucket, key) == nil {
		return
	}
	h.writeACL(w)
}

func (h *Handler) writeACL(w http.ResponseWriter) {
	result := s3xml.AccessControlPolicy{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		Owner: defaultOwner,
		AccessControlList: s3xml.ACL{
			Grants: []s3xml.Grant{{
				Grantee: s3xml.Grantee{
					Xmlns:       "http://www.w3.org/2001/XMLSchema-instance",
					Type:        "CanonicalUser",
					ID:          "cloodsys3",
					DisplayName: "cloodsys3",
				},
				Permission: "FULL_CONTROL",
			}},
		},
	}
	h.writeXML(w, http.StatusOK, result)
}

// acceptAndIgnore authenticates a write on a bucket sub-resource we do not
// model (ACL, tagging, policy, encryption) and returns the given status.
func (h *Handler) acceptAndIgnore(w http.ResponseWriter, r *http.Request, status int, needObject bool) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	if !h.checkWriteAccess(w, r, cred) {
		return
	}
	bucketName, key := getBucketAndKey(r)
	bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	if needObject && h.lookupObject(w, r, bucket, key) == nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(r.Body, maxXMLBodySize))
	w.WriteHeader(status)
}

// PutBucketAcl / PutObjectAcl — accept and ignore
func (h *Handler) PutBucketAcl(w http.ResponseWriter, r *http.Request) {
	h.acceptAndIgnore(w, r, http.StatusOK, false)
}

func (h *Handler) PutObjectAcl(w http.ResponseWriter, r *http.Request) {
	h.acceptAndIgnore(w, r, http.StatusOK, true)
}

// --- 5. Conditional request helpers ---

// checkConditional evaluates If-Match / If-None-Match / If-Modified-Since /
// If-Unmodified-Since in RFC 7232 order for an object row.
func (h *Handler) checkConditional(w http.ResponseWriter, r *http.Request, meta *db.ObjectMeta) bool {
	return h.checkConditionalETag(w, r, meta.ETag, meta.LastModified)
}

func (h *Handler) checkConditionalETag(w http.ResponseWriter, r *http.Request, etag string, lastModified time.Time) bool {
	lm := lastModified.UTC().Truncate(time.Second)
	fail := func(status int) bool {
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", lastModified.UTC().Format(http.TimeFormat))
		if status == http.StatusPreconditionFailed {
			s3err.WriteError(w, r, s3err.ErrPreconditionFailed)
		} else {
			w.WriteHeader(status)
		}
		return false
	}

	if im := r.Header.Get("If-Match"); im != "" {
		if !etagMatch(im, etag) {
			return fail(http.StatusPreconditionFailed)
		}
	} else if ius := r.Header.Get("If-Unmodified-Since"); ius != "" {
		if t, err := http.ParseTime(ius); err == nil && lm.After(t) {
			return fail(http.StatusPreconditionFailed)
		}
	}

	if inm := r.Header.Get("If-None-Match"); inm != "" {
		if etagMatch(inm, etag) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				return fail(http.StatusNotModified)
			}
			return fail(http.StatusPreconditionFailed)
		}
	} else if ims := r.Header.Get("If-Modified-Since"); ims != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		if t, err := http.ParseTime(ims); err == nil && !lm.After(t) {
			return fail(http.StatusNotModified)
		}
	}
	return true
}

// CheckConditionalHeaders is kept for callers outside this package.
func (h *Handler) CheckConditionalHeaders(w http.ResponseWriter, r *http.Request, etag string, lastModified time.Time) bool {
	return h.checkConditionalETag(w, r, etag, lastModified)
}

func etagMatch(header, etag string) bool {
	if strings.TrimSpace(header) == "*" {
		return true
	}
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		s = strings.TrimPrefix(s, "W/")
		return strings.Trim(s, "\"")
	}
	want := norm(etag)
	for _, candidate := range strings.Split(header, ",") {
		if norm(candidate) == want {
			return true
		}
	}
	return false
}

// --- 6. UploadPartCopy ---

func (h *Handler) UploadPartCopy(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	if !h.checkWriteAccess(w, r, cred) {
		return
	}

	bucketName, _ := getBucketAndKey(r)
	bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	uploadID := r.URL.Query().Get("uploadId")
	partNumber, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || partNumber < 1 || partNumber > maxParts {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}

	upload, err := h.DB.GetMultipartUpload(uploadID)
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	if upload == nil || upload.BucketID != bucket.ID {
		s3err.WriteError(w, r, s3err.ErrNoSuchUpload)
		return
	}

	srcBucketName, srcKey, srcVersion, err := parseCopySource(r.Header.Get("X-Amz-Copy-Source"))
	if err != nil || !isValidBucketName(srcBucketName) || !isValidObjectKey(srcKey) {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}
	// The credential must own the source bucket too (one credential = one bucket).
	srcBucket, ok := h.checkBucketAccess(w, r, cred, srcBucketName)
	if !ok {
		return
	}
	var srcMeta *db.ObjectMeta
	if srcVersion != "" {
		srcMeta, err = h.DB.GetObjectMetaByVersion(srcBucket.ID, srcKey, srcVersion)
	} else {
		srcMeta, err = h.DB.GetObjectMeta(srcBucket.ID, srcKey)
	}
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	if srcMeta == nil || srcMeta.IsDeleteMarker {
		s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		return
	}
	if v := r.Header.Get("x-amz-copy-source-if-match"); v != "" && !etagMatch(v, srcMeta.ETag) {
		s3err.WriteError(w, r, s3err.ErrPreconditionFailed)
		return
	}
	if v := r.Header.Get("x-amz-copy-source-if-none-match"); v != "" && etagMatch(v, srcMeta.ETag) {
		s3err.WriteError(w, r, s3err.ErrPreconditionFailed)
		return
	}

	reader, err := h.Objects.Open(srcBucket, srcMeta)
	if err != nil {
		s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		return
	}
	defer reader.Close()

	var srcReader io.Reader = reader
	expected := srcMeta.Size
	if copyRange := r.Header.Get("X-Amz-Copy-Source-Range"); copyRange != "" {
		start, end, ok := parseRange(copyRange, srcMeta.Size)
		if !ok {
			s3err.WriteError(w, r, s3err.ErrInvalidRange)
			return
		}
		if seeker, ok := reader.(io.Seeker); ok {
			seeker.Seek(start, io.SeekStart)
		} else {
			io.CopyN(io.Discard, reader, start)
		}
		expected = end - start + 1
		srcReader = io.LimitReader(reader, expected)
	}
	if expected > maxPartSize {
		s3err.WriteError(w, r, s3err.ErrEntityTooLarge)
		return
	}

	size, etag, err := h.Storage.PutMultipartPart(bucketName, uploadID, partNumber, srcReader, storage.PutOptions{MaxSize: maxPartSize})
	if err != nil {
		h.writeServiceError(w, r, err, "upload part copy error")
		return
	}

	if err := h.DB.PutMultipartPart(&db.MultipartPart{UploadID: uploadID, PartNumber: partNumber, Size: size, ETag: etag}); err != nil {
		h.Storage.DeleteMultipartPart(bucketName, uploadID, partNumber)
		h.Logger.Error("failed to save part metadata", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	if srcBucket.Versioning != "" || srcMeta.VersionID != "" {
		w.Header().Set("x-amz-copy-source-version-id", apiVersionID(srcMeta.VersionID))
	}
	h.writeXML(w, http.StatusOK, s3xml.CopyPartResult{
		Xmlns:        "http://s3.amazonaws.com/doc/2006-03-01/",
		ETag:         etag,
		LastModified: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	})
}

// --- Encryption stub ---

func (h *Handler) GetBucketEncryption(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	// Data is stored unencrypted; report that honestly (this is what S3 returns
	// for a bucket with no default-encryption configuration).
	s3err.WriteError(w, r, s3err.S3Error{
		Code:       "ServerSideEncryptionConfigurationNotFoundError",
		Message:    "The server side encryption configuration was not found",
		HTTPStatus: http.StatusNotFound,
	})
}

func (h *Handler) PutBucketEncryption(w http.ResponseWriter, r *http.Request) {
	h.acceptAndIgnore(w, r, http.StatusOK, false)
}

func (h *Handler) DeleteBucketEncryption(w http.ResponseWriter, r *http.Request) {
	h.acceptAndIgnore(w, r, http.StatusNoContent, false)
}

// --- Tagging stubs ---

func (h *Handler) GetBucketTagging(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	// No tags — return NoSuchTagSet (this is what real S3 does)
	s3err.WriteError(w, r, s3err.S3Error{
		Code:       "NoSuchTagSet",
		Message:    "The TagSet does not exist",
		HTTPStatus: http.StatusNotFound,
	})
}

func (h *Handler) PutBucketTagging(w http.ResponseWriter, r *http.Request) {
	h.acceptAndIgnore(w, r, http.StatusNoContent, false)
}

func (h *Handler) DeleteBucketTagging(w http.ResponseWriter, r *http.Request) {
	h.acceptAndIgnore(w, r, http.StatusNoContent, false)
}

func (h *Handler) GetObjectTagging(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, key := getBucketAndKey(r)
	bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	if h.lookupObject(w, r, bucket, key) == nil {
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><TagSet></TagSet></Tagging>`))
}

func (h *Handler) PutObjectTagging(w http.ResponseWriter, r *http.Request) {
	h.acceptAndIgnore(w, r, http.StatusOK, true)
}

func (h *Handler) DeleteObjectTagging(w http.ResponseWriter, r *http.Request) {
	h.acceptAndIgnore(w, r, http.StatusNoContent, true)
}

// --- Policy stub ---

func (h *Handler) GetBucketPolicy(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	s3err.WriteError(w, r, s3err.S3Error{
		Code:       "NoSuchBucketPolicy",
		Message:    "The bucket policy does not exist",
		HTTPStatus: http.StatusNotFound,
	})
}

func (h *Handler) PutBucketPolicy(w http.ResponseWriter, r *http.Request) {
	h.acceptAndIgnore(w, r, http.StatusNoContent, false)
}

func (h *Handler) DeleteBucketPolicy(w http.ResponseWriter, r *http.Request) {
	h.acceptAndIgnore(w, r, http.StatusNoContent, false)
}

// --- CORS stub (S3 semantics: no configuration stored) ---

func (h *Handler) GetBucketCors(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	s3err.WriteError(w, r, s3err.S3Error{
		Code:       "NoSuchCORSConfiguration",
		Message:    "The CORS configuration does not exist",
		HTTPStatus: http.StatusNotFound,
	})
}

func (h *Handler) PutBucketCors(w http.ResponseWriter, r *http.Request) {
	h.acceptAndIgnore(w, r, http.StatusOK, false)
}

func (h *Handler) DeleteBucketCors(w http.ResponseWriter, r *http.Request) {
	h.acceptAndIgnore(w, r, http.StatusNoContent, false)
}

// --- EncodingType helper ---

// EncodeKeyIfNeeded applies S3's encoding-type=url rules: every character that
// is not unreserved is percent-encoded (so "+" becomes "%2B"), "/" is kept.
func EncodeKeyIfNeeded(key, encodingType string) string {
	if encodingType != "url" {
		return key
	}
	segments := strings.Split(key, "/")
	for i, seg := range segments {
		segments[i] = strings.ReplaceAll(url.QueryEscape(seg), "+", "%20")
	}
	return strings.Join(segments, "/")
}
