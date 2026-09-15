package handler

import (
	"net/http"
	"strings"

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

	result := s3xml.ListVersionsResult{
		Xmlns:           "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:            bucketName,
		Prefix:          EncodeKeyIfNeeded(prefix, encodingType),
		KeyMarker:       EncodeKeyIfNeeded(keyMarker, encodingType),
		VersionIdMarker: versionMarker,
		MaxKeys:         maxKeys,
		Delimiter:       EncodeKeyIfNeeded(delimiter, encodingType),
	}
	if encodingType == "url" {
		result.EncodingType = "url"
	}

	// Delimiter roll-up is done over the version stream: every row under a
	// common prefix is skipped and the prefix counts as one entry.
	seenPrefixes := map[string]bool{}
	count := 0
	km, vm := keyMarker, versionMarker
	var lastKey, lastVersion string
	truncated := false

	for count < maxKeys && !truncated {
		versions, more, err := h.DB.ListObjectVersions(bucket.ID, prefix, km, vm, 1000)
		if err != nil {
			h.Logger.Error("failed to list object versions", "error", err)
			s3err.WriteError(w, r, s3err.ErrInternalError)
			return
		}
		if len(versions) == 0 {
			break
		}
		for _, v := range versions {
			if count >= maxKeys {
				truncated = true
				break
			}
			rest := v.Key[len(prefix):]
			if delimiter != "" {
				if idx := strings.Index(rest, delimiter); idx >= 0 {
					cp := prefix + rest[:idx+len(delimiter)]
					if !seenPrefixes[cp] {
						seenPrefixes[cp] = true
						result.CommonPrefixes = append(result.CommonPrefixes, s3xml.CommonPrefix{Prefix: EncodeKeyIfNeeded(cp, encodingType)})
						count++
						lastKey, lastVersion = v.Key, apiVersionID(v.VersionID)
					}
					continue
				}
			}
			count++
			lastKey, lastVersion = v.Key, apiVersionID(v.VersionID)
			if v.IsDeleteMarker {
				result.DeleteMarkers = append(result.DeleteMarkers, s3xml.DeleteMarkerEntry{
					Key:          EncodeKeyIfNeeded(v.Key, encodingType),
					VersionId:    apiVersionID(v.VersionID),
					IsLatest:     v.IsLatest,
					LastModified: v.LastModified.UTC().Format(lastModifiedFormat),
					Owner:        defaultOwner,
				})
			} else {
				result.Versions = append(result.Versions, s3xml.VersionEntry{
					Key:          EncodeKeyIfNeeded(v.Key, encodingType),
					VersionId:    apiVersionID(v.VersionID),
					IsLatest:     v.IsLatest,
					LastModified: v.LastModified.UTC().Format(lastModifiedFormat),
					ETag:         v.ETag,
					Size:         v.Size,
					StorageClass: "STANDARD",
					Owner:        defaultOwner,
				})
			}
		}
		if truncated {
			break
		}
		if !more {
			break
		}
		last := versions[len(versions)-1]
		km, vm = last.Key, apiVersionID(last.VersionID)
	}

	if truncated {
		result.IsTruncated = true
		result.NextKeyMarker = EncodeKeyIfNeeded(lastKey, encodingType)
		result.NextVersionIdMarker = lastVersion
	}

	h.writeXML(w, http.StatusOK, result)
}
