package s3xml

import "encoding/xml"

// ListAllMyBucketsResult is the response for ListBuckets
type ListAllMyBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Xmlns   string   `xml:"xmlns,attr"`
	Owner   Owner    `xml:"Owner"`
	Buckets Buckets  `xml:"Buckets"`
}

type Owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type Buckets struct {
	Bucket []BucketInfo `xml:"Bucket"`
}

type BucketInfo struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

// ListBucketResult is the response for ListObjects (v1)
type ListBucketResult struct {
	XMLName        xml.Name  `xml:"ListBucketResult"`
	Xmlns          string    `xml:"xmlns,attr"`
	Name           string    `xml:"Name"`
	Prefix         string    `xml:"Prefix"`
	Marker         string    `xml:"Marker"`
	MaxKeys        int       `xml:"MaxKeys"`
	IsTruncated    bool      `xml:"IsTruncated"`
	Contents       []Object  `xml:"Contents"`
	CommonPrefixes []CommonPrefix `xml:"CommonPrefixes,omitempty"`
	Delimiter      string    `xml:"Delimiter,omitempty"`
	NextMarker     string    `xml:"NextMarker,omitempty"`
}

// ListBucketResultV2 is the response for ListObjectsV2
type ListBucketResultV2 struct {
	XMLName               xml.Name  `xml:"ListBucketResult"`
	Xmlns                 string    `xml:"xmlns,attr"`
	Name                  string    `xml:"Name"`
	Prefix                string    `xml:"Prefix"`
	MaxKeys               int       `xml:"MaxKeys"`
	IsTruncated           bool      `xml:"IsTruncated"`
	Contents              []Object  `xml:"Contents"`
	CommonPrefixes        []CommonPrefix `xml:"CommonPrefixes,omitempty"`
	Delimiter             string    `xml:"Delimiter,omitempty"`
	KeyCount              int       `xml:"KeyCount"`
	ContinuationToken     string    `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
	StartAfter            string    `xml:"StartAfter,omitempty"`
}

type Object struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type CommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// CopyObjectResult
type CopyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

// InitiateMultipartUploadResult
type InitiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadId string   `xml:"UploadId"`
}

// CompleteMultipartUploadResult
type CompleteMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// CompleteMultipartUpload request body
type CompleteMultipartUpload struct {
	XMLName xml.Name       `xml:"CompleteMultipartUpload"`
	Parts   []CompletePart `xml:"Part"`
}

type CompletePart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

// DeleteObjects request and response
type DeleteRequest struct {
	XMLName xml.Name       `xml:"Delete"`
	Objects []DeleteObject `xml:"Object"`
	Quiet   bool           `xml:"Quiet"`
}

type DeleteObject struct {
	Key       string `xml:"Key"`
	VersionId string `xml:"VersionId,omitempty"`
}

type DeleteResult struct {
	XMLName xml.Name       `xml:"DeleteResult"`
	Xmlns   string         `xml:"xmlns,attr"`
	Deleted []DeletedObject `xml:"Deleted,omitempty"`
	Errors  []DeleteError  `xml:"Error,omitempty"`
}

type DeletedObject struct {
	Key string `xml:"Key"`
}

type DeleteError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// LocationConstraint for CreateBucket response
type LocationConstraint struct {
	XMLName  xml.Name `xml:"CreateBucketConfiguration"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"LocationConstraint"`
}

// ListVersionsResult is the response for ListObjectVersions
type ListVersionsResult struct {
	XMLName              xml.Name            `xml:"ListVersionsResult"`
	Xmlns                string              `xml:"xmlns,attr"`
	Name                 string              `xml:"Name"`
	Prefix               string              `xml:"Prefix"`
	KeyMarker            string              `xml:"KeyMarker"`
	NextKeyMarker        string              `xml:"NextKeyMarker,omitempty"`
	NextVersionIdMarker  string              `xml:"NextVersionIdMarker,omitempty"`
	MaxKeys              int                 `xml:"MaxKeys"`
	IsTruncated          bool                `xml:"IsTruncated"`
	Versions             []VersionEntry      `xml:"Version,omitempty"`
	DeleteMarkers        []DeleteMarkerEntry `xml:"DeleteMarker,omitempty"`
}

type VersionEntry struct {
	Key          string `xml:"Key"`
	VersionId    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type DeleteMarkerEntry struct {
	Key          string `xml:"Key"`
	VersionId    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
}

// ListMultipartUploadsResult
type ListMultipartUploadsResult struct {
	XMLName            xml.Name       `xml:"ListMultipartUploadsResult"`
	Xmlns              string         `xml:"xmlns,attr"`
	Bucket             string         `xml:"Bucket"`
	KeyMarker          string         `xml:"KeyMarker"`
	UploadIdMarker     string         `xml:"UploadIdMarker"`
	NextKeyMarker      string         `xml:"NextKeyMarker,omitempty"`
	NextUploadIdMarker string         `xml:"NextUploadIdMarker,omitempty"`
	MaxUploads         int            `xml:"MaxUploads"`
	IsTruncated        bool           `xml:"IsTruncated"`
	Uploads            []MultipartUploadEntry `xml:"Upload,omitempty"`
	Prefix             string         `xml:"Prefix"`
	Delimiter          string         `xml:"Delimiter,omitempty"`
}

type MultipartUploadEntry struct {
	Key          string `xml:"Key"`
	UploadId     string `xml:"UploadId"`
	Initiated    string `xml:"Initiated"`
	StorageClass string `xml:"StorageClass"`
}

// ListPartsResult
type ListPartsResult struct {
	XMLName              xml.Name    `xml:"ListPartsResult"`
	Xmlns                string      `xml:"xmlns,attr"`
	Bucket               string      `xml:"Bucket"`
	Key                  string      `xml:"Key"`
	UploadId             string      `xml:"UploadId"`
	MaxParts             int         `xml:"MaxParts"`
	IsTruncated          bool        `xml:"IsTruncated"`
	Parts                []PartEntry `xml:"Part,omitempty"`
}

type PartEntry struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

// LocationResult for GetBucketLocation
type LocationResult struct {
	XMLName            xml.Name `xml:"LocationConstraint"`
	Xmlns              string   `xml:"xmlns,attr"`
	LocationConstraint string   `xml:",chardata"`
}

// AccessControlPolicy for ACL stubs
type AccessControlPolicy struct {
	XMLName           xml.Name `xml:"AccessControlPolicy"`
	Xmlns             string   `xml:"xmlns,attr"`
	Owner             Owner    `xml:"Owner"`
	AccessControlList ACL      `xml:"AccessControlList"`
}

type ACL struct {
	Grants []Grant `xml:"Grant"`
}

type Grant struct {
	Grantee Grantee `xml:"Grantee"`
	Permission string `xml:"Permission"`
}

type Grantee struct {
	Xmlns       string `xml:"xmlns:xsi,attr"`
	Type        string `xml:"xsi:type,attr"`
	ID          string `xml:"ID,omitempty"`
	DisplayName string `xml:"DisplayName,omitempty"`
}

// LifecycleConfiguration for bucket lifecycle management
type LifecycleConfiguration struct {
	XMLName xml.Name        `xml:"LifecycleConfiguration"`
	Xmlns   string          `xml:"xmlns,attr,omitempty"`
	Rules   []LifecycleRule `xml:"Rule"`
}

type LifecycleRule struct {
	ID         string              `xml:"ID,omitempty"`
	Prefix     string              `xml:"Prefix"`
	Status     string              `xml:"Status"`
	Expiration LifecycleExpiration `xml:"Expiration"`
}

type LifecycleExpiration struct {
	Days int `xml:"Days"`
}

// NotificationConfiguration for bucket webhook notifications
type NotificationConfiguration struct {
	XMLName  xml.Name               `xml:"NotificationConfiguration"`
	Xmlns    string                 `xml:"xmlns,attr,omitempty"`
	Webhooks []WebhookConfiguration `xml:"WebhookConfiguration,omitempty"`
}

type WebhookConfiguration struct {
	ID         int64  `xml:"Id,omitempty"`
	URL        string `xml:"Url"`
	EventTypes string `xml:"EventTypes,omitempty"`
	Secret     string `xml:"Secret,omitempty"`
	Active     bool   `xml:"Active,omitempty"`
}
