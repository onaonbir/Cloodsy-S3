package handler

import (
	"net/http"
	"strconv"

	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/s3xml"
)

// ListObjectVersions handles GET /<bucket>?versions
func (h *Handler) ListObjectVersions(w http.ResponseWriter, r *http.Request) {
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
	versionMarker := query.Get("version-id-marker")

	maxKeys := 1000
	if mk := query.Get("max-keys"); mk != "" {
		if v, err := strconv.Atoi(mk); err == nil && v > 0 {
			maxKeys = v
		}
	}
	if maxKeys > 1000 {
		maxKeys = 1000
	}

	versions, isTruncated, err := h.DB.ListObjectVersions(bucket.ID, prefix, keyMarker, versionMarker, maxKeys)
	if err != nil {
		h.Logger.Error("failed to list object versions", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	result := s3xml.ListVersionsResult{
		Xmlns:       "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:        bucketName,
		Prefix:      prefix,
		KeyMarker:   keyMarker,
		MaxKeys:     maxKeys,
		IsTruncated: isTruncated,
	}

	if isTruncated && len(versions) > 0 {
		last := versions[len(versions)-1]
		result.NextKeyMarker = last.Key
		result.NextVersionIdMarker = last.VersionID
	}

	for _, v := range versions {
		if v.IsDeleteMarker {
			result.DeleteMarkers = append(result.DeleteMarkers, s3xml.DeleteMarkerEntry{
				Key:          v.Key,
				VersionId:    v.VersionID,
				IsLatest:     v.IsLatest,
				LastModified: v.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"),
			})
		} else {
			result.Versions = append(result.Versions, s3xml.VersionEntry{
				Key:          v.Key,
				VersionId:    v.VersionID,
				IsLatest:     v.IsLatest,
				LastModified: v.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"),
				ETag:         v.ETag,
				Size:         v.Size,
				StorageClass: "STANDARD",
			})
		}
	}

	h.writeXML(w, http.StatusOK, result)
}
