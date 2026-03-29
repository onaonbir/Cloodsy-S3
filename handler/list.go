package handler

import (
	"net/http"
	"strconv"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/s3xml"
)

// ListObjects handles GET /<bucket> (v1 and v2)
func (h *Handler) ListObjects(w http.ResponseWriter, r *http.Request) {
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

	// Check if this is ListObjectsV2
	if query.Get("list-type") == "2" {
		h.listObjectsV2(w, r, bucket)
		return
	}

	prefix := query.Get("prefix")
	marker := query.Get("marker")
	delimiter := query.Get("delimiter")
	encodingType := query.Get("encoding-type")
	if len(delimiter) > 1 {
		delimiter = delimiter[:1] // S3 delimiter is a single character
	}
	maxKeysStr := query.Get("max-keys")

	maxKeys := 1000
	if maxKeysStr != "" {
		if mk, err := strconv.Atoi(maxKeysStr); err == nil && mk > 0 {
			maxKeys = mk
		}
	}
	if maxKeys > 1000 {
		maxKeys = 1000
	}

	objects, commonPrefixes, isTruncated, nextMarker, err := h.DB.ListObjectsMeta(bucket.ID, prefix, marker, delimiter, maxKeys)
	if err != nil {
		h.Logger.Error("failed to list objects", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	result := s3xml.ListBucketResult{
		Xmlns:       "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:        bucketName,
		Prefix:      prefix,
		Marker:      marker,
		MaxKeys:     maxKeys,
		IsTruncated: isTruncated,
		Delimiter:   delimiter,
		NextMarker:  nextMarker,
	}

	for _, obj := range objects {
		result.Contents = append(result.Contents, s3xml.Object{
			Key:          EncodeKeyIfNeeded(obj.Key, encodingType),
			LastModified: obj.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         obj.ETag,
			Size:         obj.Size,
			StorageClass: "STANDARD",
		})
	}

	for _, cp := range commonPrefixes {
		result.CommonPrefixes = append(result.CommonPrefixes, s3xml.CommonPrefix{
			Prefix: EncodeKeyIfNeeded(cp, encodingType),
		})
	}

	h.writeXML(w, http.StatusOK, result)
}

func (h *Handler) listObjectsV2(w http.ResponseWriter, r *http.Request, bucket *db.Bucket) {
	query := r.URL.Query()

	prefix := query.Get("prefix")
	delimiter := query.Get("delimiter")
	encodingType := query.Get("encoding-type")
	if len(delimiter) > 1 {
		delimiter = delimiter[:1]
	}
	startAfter := query.Get("start-after")
	continuationToken := query.Get("continuation-token")
	maxKeysStr := query.Get("max-keys")

	maxKeys := 1000
	if maxKeysStr != "" {
		if mk, err := strconv.Atoi(maxKeysStr); err == nil && mk > 0 {
			maxKeys = mk
		}
	}
	if maxKeys > 1000 {
		maxKeys = 1000
	}

	objects, commonPrefixes, isTruncated, nextToken, err := h.DB.ListObjectsMetaV2(bucket.ID, prefix, startAfter, continuationToken, delimiter, maxKeys)
	if err != nil {
		h.Logger.Error("failed to list objects v2", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	result := s3xml.ListBucketResultV2{
		Xmlns:                 "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:                  bucket.Name,
		Prefix:                prefix,
		MaxKeys:               maxKeys,
		IsTruncated:           isTruncated,
		Delimiter:             delimiter,
		KeyCount:              len(objects) + len(commonPrefixes),
		ContinuationToken:     continuationToken,
		NextContinuationToken: nextToken,
		StartAfter:            startAfter,
	}

	for _, obj := range objects {
		result.Contents = append(result.Contents, s3xml.Object{
			Key:          EncodeKeyIfNeeded(obj.Key, encodingType),
			LastModified: obj.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         obj.ETag,
			Size:         obj.Size,
			StorageClass: "STANDARD",
		})
	}

	for _, cp := range commonPrefixes {
		result.CommonPrefixes = append(result.CommonPrefixes, s3xml.CommonPrefix{
			Prefix: EncodeKeyIfNeeded(cp, encodingType),
		})
	}

	h.writeXML(w, http.StatusOK, result)
}
