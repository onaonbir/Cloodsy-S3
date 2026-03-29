package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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
	EventTime    string   `json:"eventTime"`
	EventName    string   `json:"eventName"`
	S3           s3Detail `json:"s3"`
}

type s3Detail struct {
	Bucket s3Bucket `json:"bucket"`
	Object s3Object `json:"object"`
}

type s3Bucket struct {
	Name string `json:"name"`
}

type s3Object struct {
	Key       string `json:"key"`
	Size      int64  `json:"size"`
	ETag      string `json:"eTag"`
	VersionID string `json:"versionId,omitempty"`
}

// Dispatcher handles async webhook delivery.
type Dispatcher struct {
	db      *db.DB
	queue   chan Event
	workers int
	client  *http.Client
	logger  *slog.Logger
	done    chan struct{}
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
		},
		logger: logger,
		done:   make(chan struct{}),
	}
	for i := 0; i < workers; i++ {
		go d.worker()
	}
	return d
}

// Emit queues an event for delivery. Non-blocking; drops event if queue is full.
func (d *Dispatcher) Emit(event Event) {
	select {
	case d.queue <- event:
	default:
		d.logger.Warn("webhook queue full, dropping event", "eventType", event.EventType, "key", event.Key)
	}
}

// Stop gracefully shuts down the dispatcher.
func (d *Dispatcher) Stop() {
	close(d.queue)
	// Wait for workers to drain
	for i := 0; i < d.workers; i++ {
		<-d.done
	}
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

func (d *Dispatcher) deliver(hook db.BucketWebhook, event Event) {
	payload := s3EventPayload{
		Records: []s3Record{{
			EventVersion: "2.1",
			EventSource:  "cloodsy:s3",
			EventTime:    event.Timestamp.Format(time.RFC3339),
			EventName:    event.EventType,
			S3: s3Detail{
				Bucket: s3Bucket{Name: event.BucketName},
				Object: s3Object{
					Key:       event.Key,
					Size:      event.Size,
					ETag:      event.ETag,
					VersionID: event.VersionID,
				},
			},
		}},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		d.logger.Error("failed to marshal webhook payload", "error", err)
		return
	}

	// Retry with exponential backoff: 1s, 2s, 4s
	backoff := time.Second
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff)
			backoff *= 2
		}

		req, err := http.NewRequest("POST", hook.URL, bytes.NewReader(body))
		if err != nil {
			d.logger.Error("failed to create webhook request", "url", hook.URL, "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")

		// HMAC signing
		if hook.Secret != "" {
			mac := hmac.New(sha256.New, []byte(hook.Secret))
			mac.Write(body)
			sig := hex.EncodeToString(mac.Sum(nil))
			req.Header.Set("X-Cloodsy-Signature", fmt.Sprintf("sha256=%s", sig))
		}

		resp, err := d.client.Do(req)
		if err != nil {
			d.logger.Warn("webhook delivery failed", "url", hook.URL, "attempt", attempt+1, "error", err)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return // success
		}
		d.logger.Warn("webhook non-2xx response", "url", hook.URL, "status", resp.StatusCode, "attempt", attempt+1)
	}

	d.logger.Error("webhook delivery failed after retries", "url", hook.URL, "bucket", hook.BucketName)
}
