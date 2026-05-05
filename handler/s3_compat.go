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
)

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
	maxUploads := 1000
	if v := query.Get("max-uploads"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			maxUploads = n
		}
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
		Prefix:         prefix,
		Delimiter:      query.Get("delimiter"),
	}

	for _, u := range uploads {
		result.Uploads = append(result.Uploads, s3xml.MultipartUploadEntry{
			Key:          u.Key,
			UploadId:     u.ID,
			Initiated:    u.CreatedAt.UTC().Format(time.RFC3339),
			StorageClass: "STANDARD",
		})
	}

	if truncated && len(uploads) > 0 {
		last := uploads[len(uploads)-1]
		result.NextKeyMarker = last.Key
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
	if err != nil || upload == nil {
		s3err.WriteError(w, r, s3err.ErrNoSuchUpload)
		return
	}

	// Validate upload belongs to the requested bucket (prevent cross-bucket hijacking)
	if upload.BucketID != bucket.ID {
		s3err.WriteError(w, r, s3err.ErrNoSuchUpload)
		return
	}

	parts, err := h.DB.ListMultipartParts(uploadID)
	if err != nil {
		h.Logger.Error("list parts error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	maxParts := 1000
	if v := r.URL.Query().Get("max-parts"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxParts = n
		}
	}

	truncated := len(parts) > maxParts
	if truncated {
		parts = parts[:maxParts]
	}

	result := s3xml.ListPartsResult{
		Xmlns:       "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:      bucketName,
		Key:         key,
		UploadId:    uploadID,
		MaxParts:    maxParts,
		IsTruncated: truncated,
	}

	for _, p := range parts {
		result.Parts = append(result.Parts, s3xml.PartEntry{
			PartNumber:   p.PartNumber,
			LastModified: p.CreatedAt.UTC().Format(time.RFC3339),
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
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	h.writeACL(w)
}

func (h *Handler) writeACL(w http.ResponseWriter) {
	result := s3xml.AccessControlPolicy{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		Owner: s3xml.Owner{ID: "cloodsys3", DisplayName: "cloodsys3"},
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

// PutBucketAcl / PutObjectAcl — accept and ignore
func (h *Handler) PutBucketAcl(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	io.Copy(io.Discard, r.Body)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) PutObjectAcl(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	io.Copy(io.Discard, r.Body)
	w.WriteHeader(http.StatusOK)
}

// --- 5. Conditional request helpers ---

// CheckConditionalHeaders checks If-Match, If-None-Match, If-Modified-Since, If-Unmodified-Since.
// Returns true if the request should proceed, false if a 304/412 was sent.
func (h *Handler) CheckConditionalHeaders(w http.ResponseWriter, r *http.Request, etag string, lastModified time.Time) bool {
	// If-Match: proceed only if ETag matches
	if im := r.Header.Get("If-Match"); im != "" {
		if !etagMatch(im, etag) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return false
		}
	}

	// If-None-Match: proceed only if ETag does NOT match (used for caching)
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		if etagMatch(inm, etag) {
			w.WriteHeader(http.StatusNotModified)
			return false
		}
	}

	// If-Modified-Since: return 304 if not modified after the given date
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		t, err := http.ParseTime(ims)
		if err == nil && !lastModified.After(t) {
			w.WriteHeader(http.StatusNotModified)
			return false
		}
	}

	// If-Unmodified-Since: return 412 if modified after the given date
	if ius := r.Header.Get("If-Unmodified-Since"); ius != "" {
		t, err := http.ParseTime(ius)
		if err == nil && lastModified.After(t) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return false
		}
	}

	return true
}

func etagMatch(header, etag string) bool {
	if header == "*" {
		return true
	}
	// Handle comma-separated list of ETags
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == etag {
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
	partNumberStr := r.URL.Query().Get("partNumber")
	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil || partNumber < 1 || partNumber > maxParts {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}

	// Validate the upload belongs to this bucket before staging any data.
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

	// Parse copy source
	copySource := r.Header.Get("X-Amz-Copy-Source")
	if copySource == "" {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}
	copySource, _ = url.PathUnescape(copySource)
	copySource = strings.TrimPrefix(copySource, "/")
	parts := strings.SplitN(copySource, "/", 2)
	if len(parts) != 2 {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}
	srcBucket, srcKey := parts[0], parts[1]

	// Get source object
	reader, err := h.Storage.GetObject(srcBucket, srcKey)
	if err != nil {
		s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		return
	}

	// Handle range copy
	var srcReader io.Reader = reader
	copyRange := r.Header.Get("X-Amz-Copy-Source-Range")
	if copyRange != "" {
		// Parse "bytes=start-end"
		rangeStr := strings.TrimPrefix(copyRange, "bytes=")
		rangeParts := strings.Split(rangeStr, "-")
		if len(rangeParts) == 2 {
			start, err1 := strconv.ParseInt(rangeParts[0], 10, 64)
			end, err2 := strconv.ParseInt(rangeParts[1], 10, 64)
			if err1 == nil && err2 == nil && start >= 0 && end >= start {
				// Skip to start
				io.CopyN(io.Discard, reader, start)
				srcReader = io.LimitReader(reader, end-start+1)
			}
		}
	}

	// Write as multipart part
	size, etag, err := h.Storage.PutMultipartPart(bucketName, uploadID, partNumber, srcReader)
	reader.Close()
	if err != nil {
		h.Logger.Error("upload part copy error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	// Save part metadata
	h.DB.PutMultipartPart(&db.MultipartPart{
		UploadID:   uploadID,
		PartNumber: partNumber,
		Size:       size,
		ETag:       etag,
	})

	// Return CopyPartResult
	result := struct {
		XMLName      string `xml:"CopyPartResult"`
		ETag         string `xml:"ETag"`
		LastModified string `xml:"LastModified"`
	}{
		ETag:         etag,
		LastModified: time.Now().UTC().Format(time.RFC3339),
	}
	h.writeXML(w, http.StatusOK, result)
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
	// Return default SSE-S3 (AES256)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<ServerSideEncryptionConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault><BucketKeyEnabled>false</BucketKeyEnabled></Rule>
</ServerSideEncryptionConfiguration>`))
}

func (h *Handler) PutBucketEncryption(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	io.Copy(io.Discard, r.Body)
	w.WriteHeader(http.StatusOK)
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
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	io.Copy(io.Discard, r.Body)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) DeleteBucketTagging(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) GetObjectTagging(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><TagSet></TagSet></Tagging>`))
}

func (h *Handler) PutObjectTagging(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	io.Copy(io.Discard, r.Body)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) DeleteObjectTagging(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	// No policy set — this is what real S3 returns
	s3err.WriteError(w, r, s3err.S3Error{
		Code:       "NoSuchBucketPolicy",
		Message:    "The bucket policy does not exist",
		HTTPStatus: http.StatusNotFound,
	})
}

func (h *Handler) PutBucketPolicy(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	io.Copy(io.Discard, r.Body)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) DeleteBucketPolicy(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- EncodingType helper ---

func EncodeKeyIfNeeded(key, encodingType string) string {
	if encodingType == "url" {
		// S3 URL encoding: encode special chars but preserve /
		segments := strings.Split(key, "/")
		for i, seg := range segments {
			segments[i] = url.PathEscape(seg)
		}
		return strings.Join(segments, "/")
	}
	return key
}
