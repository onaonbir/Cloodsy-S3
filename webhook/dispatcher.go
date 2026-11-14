package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onaonbir/Cloodsy-S3/db"
)

// Event represents an S3 event to dispatch via webhooks.
type Event struct {
	BucketName string
	EventType  string
	Key        string
	Size       int64
	ETag       string
	VersionID  string
	Timestamp  time.Time
}

// s3EventPayload is the AWS S3-compatible event notification payload.
type s3EventPayload struct {
	Records []s3Record `json:"Records"`
}

type s3Record struct {
	EventVersion string   `json:"eventVersion"`
	EventSource  string   `json:"eventSource"`
	AwsRegion    string   `json:"awsRegion"`
	EventTime    string   `json:"eventTime"`
	EventName    string   `json:"eventName"`
	S3           s3Detail `json:"s3"`
}

type s3Detail struct {
	SchemaVersion   string   `json:"s3SchemaVersion"`
	ConfigurationID string   `json:"configurationId"`
	Bucket          s3Bucket `json:"bucket"`
	Object          s3Object `json:"object"`
}

type s3Bucket struct {
	Name string `json:"name"`
	ARN  string `json:"arn"`
}

type s3Object struct {
	// Key is the raw object key (kept unencoded for backward compatibility
	// with existing consumers); UrlEncodedKey follows the AWS format.
	Key           string `json:"key"`
	URLEncodedKey string `json:"urlEncodedKey"`
	Size          int64  `json:"size"`
	ETag          string `json:"eTag"`
	VersionID     string `json:"versionId,omitempty"`
	Sequencer     string `json:"sequencer"`
}

// Dispatcher handles async webhook delivery.
type Dispatcher struct {
	db      *db.DB
	queue   chan Event
	workers int
	client  *http.Client
	logger  *slog.Logger
	region  string
	done    chan struct{}
	mu      sync.RWMutex
	closed  bool

	// Delivery log: attempts are handed to a single recorder goroutine so a
	// slow database never delays a delivery.
	records      chan db.WebhookDelivery
	recorderDone chan struct{}
}

// NewDispatcher creates a new webhook dispatcher.
func NewDispatcher(database *db.DB, workers int, logger *slog.Logger) *Dispatcher {
	if workers <= 0 {
		workers = 4
	}
	d := &Dispatcher{
		db:      database,
		queue:   make(chan Event, 1000),
		workers: workers,
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // never follow redirects (SSRF hygiene)
			},
		},
		logger: logger,
		region: "us-east-1",
		done:   make(chan struct{}),

		records:      make(chan db.WebhookDelivery, 1024),
		recorderDone: make(chan struct{}),
	}
	if database != nil {
		go d.recorder()
	} else {
		close(d.recorderDone)
	}
	for i := 0; i < workers; i++ {
		go d.worker()
	}
	return d
}

// NewSender returns a worker-less dispatcher usable only for SendTest (when
// no running dispatcher is available, e.g. an admin-only process).
func NewSender(database *db.DB, region string, logger *slog.Logger) *Dispatcher {
	d := NewDispatcher(database, 1, logger)
	d.Stop()
	if region != "" {
		d.region = region
	}
	return d
}

// SetRegion sets the awsRegion reported in payloads.
func (d *Dispatcher) SetRegion(region string) { d.region = region }

// Emit queues an event for delivery. Non-blocking; drops the event if the
// queue is full or the dispatcher has been stopped.
func (d *Dispatcher) Emit(event Event) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return
	}
	select {
	case d.queue <- event:
	default:
		d.logger.Warn("webhook queue full, dropping event", "eventType", event.EventType, "key", event.Key)
	}
}

// Stop gracefully shuts down the dispatcher and waits for workers to drain.
func (d *Dispatcher) Stop() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	close(d.queue)
	d.mu.Unlock()
	for i := 0; i < d.workers; i++ {
		<-d.done
	}
	// Workers are gone, so nothing sends on records any more.
	close(d.records)
	<-d.recorderDone
}

func (d *Dispatcher) worker() {
	defer func() { d.done <- struct{}{} }()

	for event := range d.queue {
		hooks, err := d.db.GetActiveWebhooksForBucket(event.BucketName)
		if err != nil {
			d.logger.Error("failed to get webhooks", "bucket", event.BucketName, "error", err)
			continue
		}

		for _, hook := range hooks {
			if !matchEvent(hook.EventTypes, event.EventType) {
				continue
			}
			d.deliver(hook, event)
		}
	}
}

func matchEvent(pattern, eventType string) bool {
	if pattern == "*" {
		return true
	}
	for _, p := range strings.Split(pattern, ",") {
		p = strings.TrimSpace(p)
		if p == eventType {
			return true
		}
		// Wildcard matching: "s3:ObjectCreated:*" matches "s3:ObjectCreated:Put"
		if strings.HasSuffix(p, ":*") {
			prefix := strings.TrimSuffix(p, "*")
			if strings.HasPrefix(eventType, prefix) {
				return true
			}
		}
	}
	return false
}

func newDeliveryID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// buildPayload renders the S3-compatible event body for one hook.
func (d *Dispatcher) buildPayload(hook db.BucketWebhook, event Event) ([]byte, error) {
	payload := s3EventPayload{
		Records: []s3Record{{
			EventVersion: "2.1",
			EventSource:  "cloodsy:s3",
			AwsRegion:    d.region,
			EventTime:    event.Timestamp.UTC().Format(time.RFC3339),
			EventName:    event.EventType,
			S3: s3Detail{
				SchemaVersion:   "1.0",
				ConfigurationID: hook.Name,
				Bucket:          s3Bucket{Name: event.BucketName, ARN: "arn:aws:s3:::" + event.BucketName},
				Object: s3Object{
					Key:           event.Key,
					URLEncodedKey: strings.ReplaceAll(url.QueryEscape(event.Key), "%2F", "/"),
					Size:          event.Size,
					ETag:          strings.Trim(event.ETag, "\""),
					VersionID:     event.VersionID,
					Sequencer:     fmt.Sprintf("%016X", event.Timestamp.UnixNano()),
				},
			},
		}},
	}
	return json.Marshal(payload)
}

// DeliveryResult describes one delivery attempt.
type DeliveryResult struct {
	StatusCode int    // 0 when no HTTP response was received
	Error      string // "" on a 2xx response
	Duration   time.Duration
}

// OK reports a 2xx response.
func (r DeliveryResult) OK() bool { return r.Error == "" && r.StatusCode >= 200 && r.StatusCode < 300 }

var errInvalidRequest = "invalid request: "

// send performs one signed POST of body to the hook.
func (d *Dispatcher) send(hook db.BucketWebhook, event Event, body []byte, deliveryID string) DeliveryResult {
	start := time.Now()
	req, err := http.NewRequest("POST", hook.URL, bytes.NewReader(body))
	if err != nil {
		return DeliveryResult{Error: errInvalidRequest + err.Error(), Duration: time.Since(start)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Cloodsy-S3-Webhook/1.0")
	req.Header.Set("X-Cloodsy-Delivery", deliveryID)
	req.Header.Set("X-Cloodsy-Event", event.EventType)
	req.Header.Set("X-Cloodsy-Timestamp", strconv.FormatInt(event.Timestamp.Unix(), 10))

	// HMAC signing over the body (X-Cloodsy-Signature) plus a replay-resistant
	// variant that also covers the timestamp (X-Cloodsy-Signature-256).
	if hook.Secret != "" {
		mac := hmac.New(sha256.New, []byte(hook.Secret))
		mac.Write(body)
		req.Header.Set("X-Cloodsy-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))

		mac2 := hmac.New(sha256.New, []byte(hook.Secret))
		mac2.Write([]byte(strconv.FormatInt(event.Timestamp.Unix(), 10)))
		mac2.Write([]byte("."))
		mac2.Write(body)
		req.Header.Set("X-Cloodsy-Signature-256", "t="+strconv.FormatInt(event.Timestamp.Unix(), 10)+",v1="+hex.EncodeToString(mac2.Sum(nil)))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return DeliveryResult{Error: err.Error(), Duration: time.Since(start)}
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	res := DeliveryResult{StatusCode: resp.StatusCode, Duration: time.Since(start)}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		res.Error = fmt.Sprintf("unexpected status %d", resp.StatusCode)
	}
	return res
}

func (d *Dispatcher) deliver(hook db.BucketWebhook, event Event) {
	body, err := d.buildPayload(hook, event)
	if err != nil {
		d.logger.Error("failed to marshal webhook payload", "error", err)
		return
	}
	deliveryID := newDeliveryID()

	// Retry with exponential backoff: 1s, 2s, 4s
	backoff := time.Second
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff)
			backoff *= 2
		}
		res := d.send(hook, event, body, deliveryID)
		d.record(hook, event, res, attempt+1)
		if res.OK() {
			return
		}
		switch {
		case res.StatusCode == 0 && strings.HasPrefix(res.Error, errInvalidRequest):
			d.logger.Error("failed to create webhook request", "url", hook.URL, "error", res.Error)
			return
		case res.StatusCode == 0:
			d.logger.Warn("webhook delivery failed", "url", hook.URL, "attempt", attempt+1, "error", res.Error)
		default:
			d.logger.Warn("webhook non-2xx response", "url", hook.URL, "status", res.StatusCode, "attempt", attempt+1)
		}
	}

	d.logger.Error("webhook delivery failed after retries", "url", hook.URL, "bucket", hook.BucketName)
}

// TestEventType is the event name of synthetic test deliveries.
const TestEventType = "s3:TestEvent"

// SendTest synchronously delivers one synthetic s3:TestEvent (key
// "cloodsy-test") to hook with the same payload shape, signing and timeout as
// real events, records it in the delivery log and returns the outcome. No
// retries are made.
func (d *Dispatcher) SendTest(hook db.BucketWebhook) DeliveryResult {
	event := Event{
		BucketName: hook.BucketName,
		EventType:  TestEventType,
		Key:        "cloodsy-test",
		Timestamp:  time.Now().UTC(),
	}
	body, err := d.buildPayload(hook, event)
	if err != nil {
		return DeliveryResult{Error: err.Error()}
	}
	res := d.send(hook, event, body, newDeliveryID())
	if d.db != nil {
		if err := d.db.InsertWebhookDelivery(deliveryRecord(hook, event, res, 1)); err != nil {
			d.logger.Warn("failed to record webhook delivery", "webhook", hook.ID, "error", err)
		}
	}
	return res
}

func deliveryRecord(hook db.BucketWebhook, event Event, res DeliveryResult, attempt int) db.WebhookDelivery {
	errMsg := res.Error
	if len(errMsg) > 512 {
		errMsg = errMsg[:512]
	}
	key := event.Key
	if len(key) > 1024 {
		key = key[:1024]
	}
	return db.WebhookDelivery{
		WebhookID:  hook.ID,
		EventType:  event.EventType,
		ObjectKey:  key,
		StatusCode: res.StatusCode,
		Error:      errMsg,
		DurationMs: res.Duration.Milliseconds(),
		Attempt:    attempt,
		CreatedAt:  time.Now().UTC(),
	}
}

// record queues a delivery attempt for the recorder goroutine. It never
// blocks the delivery: when the recorder falls behind, records are dropped.
func (d *Dispatcher) record(hook db.BucketWebhook, event Event, res DeliveryResult, attempt int) {
	if d.db == nil {
		return
	}
	select {
	case d.records <- deliveryRecord(hook, event, res, attempt):
	default:
	}
}

// recorder persists delivery attempts off the delivery path.
func (d *Dispatcher) recorder() {
	defer close(d.recorderDone)
	for rec := range d.records {
		if err := d.db.InsertWebhookDelivery(rec); err != nil {
			d.logger.Warn("failed to record webhook delivery", "webhook", rec.WebhookID, "error", err)
		}
	}
}

// ValidateURL checks a webhook destination. Only http/https are accepted.
// When allowPrivate is false, loopback, link-local (including the cloud
// metadata address) and private networks are rejected to prevent SSRF from
// bucket-credential holders.
func ValidateURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid webhook url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("webhook url must use http or https")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("webhook url must have a host")
	}
	if allowPrivate {
		return nil
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return errors.New("webhook url may not target localhost")
	}
	ips := []net.IP{}
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip)
	} else {
		resolved, err := net.LookupIP(host)
		if err != nil {
			// Unresolvable now (offline install, DNS hiccup): accept; delivery
			// will simply fail until the name resolves.
			return nil
		}
		ips = resolved
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() || ip.IsUnspecified() {
			return errors.New("webhook url may not target private or link-local addresses")
		}
	}
	return nil
}
