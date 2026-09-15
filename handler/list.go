package handler

import (
	"errors"
	"net/http"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/s3xml"
)

const lastModifiedFormat = "2006-01-02T15:04:05.000Z"

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
	if query.Get("list-type") == "2" {
		h.listObjectsV2(w, r, bucket)
		return
	}

	prefix := query.Get("prefix")
	marker := query.Get("marker")
	delimiter := query.Get("delimiter")
	encodingType := query.Get("encoding-type")
	if encodingType != "" && encodingType != "url" {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}
	maxKeys, ok := parseMaxKeys(query.Get("max-keys"))
	if !ok {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
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
		Prefix:      EncodeKeyIfNeeded(prefix, encodingType),
		Marker:      EncodeKeyIfNeeded(marker, encodingType),
		MaxKeys:     maxKeys,
		IsTruncated: isTruncated,
		Delimiter:   EncodeKeyIfNeeded(delimiter, encodingType),
	}
	if encodingType == "url" {
		result.EncodingType = "url"
	}
	// S3 only returns NextMarker when a delimiter is used; clients otherwise
	// continue from the last key.
	if isTruncated && delimiter != "" {
		result.NextMarker = EncodeKeyIfNeeded(nextMarker, encodingType)
	}

	for _, obj := range objects {
		result.Contents = append(result.Contents, s3xml.Object{
			Key:          EncodeKeyIfNeeded(obj.Key, encodingType),
			LastModified: obj.LastModified.UTC().Format(lastModifiedFormat),
			ETag:         obj.ETag,
			Size:         obj.Size,
			StorageClass: "STANDARD",
		})
	}
	for _, cp := range commonPrefixes {
		result.CommonPrefixes = append(result.CommonPrefixes, s3xml.CommonPrefix{Prefix: EncodeKeyIfNeeded(cp, encodingType)})
	}

	h.writeXML(w, http.StatusOK, result)
}

func (h *Handler) listObjectsV2(w http.ResponseWriter, r *http.Request, bucket *db.Bucket) {
	query := r.URL.Query()

	prefix := query.Get("prefix")
	delimiter := query.Get("delimiter")
	encodingType := query.Get("encoding-type")
	if encodingType != "" && encodingType != "url" {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}
	startAfter := query.Get("start-after")
	continuationToken := query.Get("continuation-token")
	maxKeys, ok := parseMaxKeys(query.Get("max-keys"))
	if !ok {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}

	objects, commonPrefixes, isTruncated, nextToken, err := h.DB.ListObjectsMetaV2(bucket.ID, prefix, startAfter, continuationToken, delimiter, maxKeys)
	if err != nil {
		if errors.Is(err, db.ErrInvalidContinuationToken) {
			s3err.WriteErrorMsg(w, r, s3err.ErrInvalidArgument, "The continuation token provided is incorrect")
			return
		}
		h.Logger.Error("failed to list objects v2", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	result := s3xml.ListBucketResultV2{
		Xmlns:                 "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:                  bucket.Name,
		Prefix:                EncodeKeyIfNeeded(prefix, encodingType),
		MaxKeys:               maxKeys,
		IsTruncated:           isTruncated,
		Delimiter:             EncodeKeyIfNeeded(delimiter, encodingType),
		KeyCount:              len(objects) + len(commonPrefixes),
		ContinuationToken:     continuationToken,
		NextContinuationToken: nextToken,
		StartAfter:            EncodeKeyIfNeeded(startAfter, encodingType),
	}
	if encodingType == "url" {
		result.EncodingType = "url"
	}

	for _, obj := range objects {
		result.Contents = append(result.Contents, s3xml.Object{
			Key:          EncodeKeyIfNeeded(obj.Key, encodingType),
			LastModified: obj.LastModified.UTC().Format(lastModifiedFormat),
			ETag:         obj.ETag,
			Size:         obj.Size,
			StorageClass: "STANDARD",
		})
	}
	for _, cp := range commonPrefixes {
		result.CommonPrefixes = append(result.CommonPrefixes, s3xml.CommonPrefix{Prefix: EncodeKeyIfNeeded(cp, encodingType)})
	}

	h.writeXML(w, http.StatusOK, result)
}
