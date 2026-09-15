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
	XMLName        xml.Name       `xml:"ListBucketResult"`
	Xmlns          string         `xml:"xmlns,attr"`
	Name           string         `xml:"Name"`
	Prefix         string         `xml:"Prefix"`
	Marker         string         `xml:"Marker"`
	NextMarker     string         `xml:"NextMarker,omitempty"`
	MaxKeys        int            `xml:"MaxKeys"`
	Delimiter      string         `xml:"Delimiter,omitempty"`
	EncodingType   string         `xml:"EncodingType,omitempty"`
	IsTruncated    bool           `xml:"IsTruncated"`
	Contents       []Object       `xml:"Contents"`
	CommonPrefixes []CommonPrefix `xml:"CommonPrefixes,omitempty"`
}

// ListBucketResultV2 is the response for ListObjectsV2
type ListBucketResultV2 struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	Xmlns                 string         `xml:"xmlns,attr"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	KeyCount              int            `xml:"KeyCount"`
	MaxKeys               int            `xml:"MaxKeys"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	EncodingType          string         `xml:"EncodingType,omitempty"`
	IsTruncated           bool           `xml:"IsTruncated"`
	Contents              []Object       `xml:"Contents"`
	CommonPrefixes        []CommonPrefix `xml:"CommonPrefixes,omitempty"`
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
	Xmlns        string   `xml:"xmlns,attr"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

// CopyPartResult
type CopyPartResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	Xmlns        string   `xml:"xmlns,attr"`
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
	XMLName xml.Name        `xml:"DeleteResult"`
	Xmlns   string          `xml:"xmlns,attr"`
	Deleted []DeletedObject `xml:"Deleted,omitempty"`
	Errors  []DeleteError   `xml:"Error,omitempty"`
}

type DeletedObject struct {
	Key                   string `xml:"Key"`
	VersionId             string `xml:"VersionId,omitempty"`
	DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
	DeleteMarkerVersionId string `xml:"DeleteMarkerVersionId,omitempty"`
}

type DeleteError struct {
	Key       string `xml:"Key"`
	VersionId string `xml:"VersionId,omitempty"`
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
}

// LocationConstraint for CreateBucket response
type LocationConstraint struct {
	XMLName  xml.Name `xml:"CreateBucketConfiguration"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"LocationConstraint"`
}

// ListVersionsResult is the response for ListObjectVersions
type ListVersionsResult struct {
	XMLName             xml.Name            `xml:"ListVersionsResult"`
	Xmlns               string              `xml:"xmlns,attr"`
	Name                string              `xml:"Name"`
	Prefix              string              `xml:"Prefix"`
	KeyMarker           string              `xml:"KeyMarker"`
	VersionIdMarker     string              `xml:"VersionIdMarker"`
	NextKeyMarker       string              `xml:"NextKeyMarker,omitempty"`
	NextVersionIdMarker string              `xml:"NextVersionIdMarker,omitempty"`
	MaxKeys             int                 `xml:"MaxKeys"`
	Delimiter           string              `xml:"Delimiter,omitempty"`
	EncodingType        string              `xml:"EncodingType,omitempty"`
	IsTruncated         bool                `xml:"IsTruncated"`
	Versions            []VersionEntry      `xml:"Version,omitempty"`
	DeleteMarkers       []DeleteMarkerEntry `xml:"DeleteMarker,omitempty"`
	CommonPrefixes      []CommonPrefix      `xml:"CommonPrefixes,omitempty"`
}

type VersionEntry struct {
	Key          string `xml:"Key"`
	VersionId    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
	Owner        Owner  `xml:"Owner"`
}

type DeleteMarkerEntry struct {
	Key          string `xml:"Key"`
	VersionId    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	Owner        Owner  `xml:"Owner"`
}

// ListMultipartUploadsResult
type ListMultipartUploadsResult struct {
	XMLName            xml.Name               `xml:"ListMultipartUploadsResult"`
	Xmlns              string                 `xml:"xmlns,attr"`
	Bucket             string                 `xml:"Bucket"`
	KeyMarker          string                 `xml:"KeyMarker"`
	UploadIdMarker     string                 `xml:"UploadIdMarker"`
	NextKeyMarker      string                 `xml:"NextKeyMarker,omitempty"`
	NextUploadIdMarker string                 `xml:"NextUploadIdMarker,omitempty"`
	MaxUploads         int                    `xml:"MaxUploads"`
	IsTruncated        bool                   `xml:"IsTruncated"`
	Uploads            []MultipartUploadEntry `xml:"Upload,omitempty"`
	Prefix             string                 `xml:"Prefix"`
	Delimiter          string                 `xml:"Delimiter,omitempty"`
	CommonPrefixes     []CommonPrefix         `xml:"CommonPrefixes,omitempty"`
	EncodingType       string                 `xml:"EncodingType,omitempty"`
}

type MultipartUploadEntry struct {
	Key          string `xml:"Key"`
	UploadId     string `xml:"UploadId"`
	Initiator    Owner  `xml:"Initiator"`
	Owner        Owner  `xml:"Owner"`
	StorageClass string `xml:"StorageClass"`
	Initiated    string `xml:"Initiated"`
}

// ListPartsResult
type ListPartsResult struct {
	XMLName              xml.Name    `xml:"ListPartsResult"`
	Xmlns                string      `xml:"xmlns,attr"`
	Bucket               string      `xml:"Bucket"`
	Key                  string      `xml:"Key"`
	UploadId             string      `xml:"UploadId"`
	Initiator            Owner       `xml:"Initiator"`
	Owner                Owner       `xml:"Owner"`
	StorageClass         string      `xml:"StorageClass"`
	PartNumberMarker     int         `xml:"PartNumberMarker"`
	NextPartNumberMarker int         `xml:"NextPartNumberMarker,omitempty"`
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
	Grantee    Grantee `xml:"Grantee"`
	Permission string  `xml:"Permission"`
}

type Grantee struct {
	Xmlns       string `xml:"xmlns:xsi,attr"`
	Type        string `xml:"xsi:type,attr"`
	ID          string `xml:"ID,omitempty"`
	DisplayName string `xml:"DisplayName,omitempty"`
}

// LifecycleConfiguration for bucket lifecycle management. Both the legacy
// top-level <Prefix> and the current <Filter> forms are accepted.
type LifecycleConfiguration struct {
	XMLName xml.Name        `xml:"LifecycleConfiguration"`
	Xmlns   string          `xml:"xmlns,attr,omitempty"`
	Rules   []LifecycleRule `xml:"Rule"`
}

type LifecycleRule struct {
	ID                             string                          `xml:"ID,omitempty"`
	Prefix                         *string                         `xml:"Prefix,omitempty"`
	Filter                         *LifecycleFilter                `xml:"Filter,omitempty"`
	Status                         string                          `xml:"Status"`
	Expiration                     *LifecycleExpiration            `xml:"Expiration,omitempty"`
	NoncurrentVersionExpiration    *NoncurrentVersionExpiration    `xml:"NoncurrentVersionExpiration,omitempty"`
	AbortIncompleteMultipartUpload *AbortIncompleteMultipartUpload `xml:"AbortIncompleteMultipartUpload,omitempty"`
	Transitions                    []LifecycleTransition           `xml:"Transition,omitempty"`
}

type LifecycleFilter struct {
	Prefix *string           `xml:"Prefix,omitempty"`
	And    *LifecycleAnd     `xml:"And,omitempty"`
	Tag    *LifecycleTag     `xml:"Tag,omitempty"`
	Size   *LifecycleSizeAny `xml:",any,omitempty"`
}

// LifecycleSizeAny swallows ObjectSizeGreaterThan/LessThan so the decoder
// does not choke on them; they are not enforced.
type LifecycleSizeAny struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type LifecycleAnd struct {
	Prefix *string        `xml:"Prefix,omitempty"`
	Tags   []LifecycleTag `xml:"Tag,omitempty"`
}

type LifecycleTag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

type LifecycleExpiration struct {
	Days                      int    `xml:"Days,omitempty"`
	Date                      string `xml:"Date,omitempty"`
	ExpiredObjectDeleteMarker bool   `xml:"ExpiredObjectDeleteMarker,omitempty"`
}

type NoncurrentVersionExpiration struct {
	NoncurrentDays          int `xml:"NoncurrentDays,omitempty"`
	NewerNoncurrentVersions int `xml:"NewerNoncurrentVersions,omitempty"`
}

type AbortIncompleteMultipartUpload struct {
	DaysAfterInitiation int `xml:"DaysAfterInitiation"`
}

type LifecycleTransition struct {
	Days         int    `xml:"Days,omitempty"`
	Date         string `xml:"Date,omitempty"`
	StorageClass string `xml:"StorageClass,omitempty"`
}

// NotificationConfiguration for bucket webhook notifications
type NotificationConfiguration struct {
	XMLName  xml.Name               `xml:"NotificationConfiguration"`
	Xmlns    string                 `xml:"xmlns,attr,omitempty"`
	Webhooks []WebhookConfiguration `xml:"WebhookConfiguration,omitempty"`
}

type WebhookConfiguration struct {
	ID         int64  `xml:"Id,omitempty"`
	Name       string `xml:"Name,omitempty"`
	URL        string `xml:"Url"`
	EventTypes string `xml:"EventTypes,omitempty"`
	Secret     string `xml:"Secret,omitempty"`
	Active     bool   `xml:"Active,omitempty"`
}
