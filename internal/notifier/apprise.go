package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultAppriseTimeout bounds requests to the Apprise API when the caller
// doesn't supply a client with its own timeout. Dispatcher.Dispatch has no
// per-notifier deadline of its own, so an unbounded client.Timeout (the
// zero-value default on http.DefaultClient) would let a hung Apprise
// endpoint block dispatch indefinitely.
const defaultAppriseTimeout = 15 * time.Second

// AppriseConfig holds connection settings for an Apprise API endpoint.
type AppriseConfig struct {
	// URL is the base Apprise API endpoint (e.g. http://apprise:8000/notify).
	URL string
	// ServiceURL is the user's notification destination (e.g. slack://..., ntfy://...).
	ServiceURL string
	EventMask  []EventType
}

// AppriseNotifier posts events to an Apprise HTTP API endpoint.
type AppriseNotifier struct {
	cfg    AppriseConfig
	client *http.Client
}

// NewAppriseNotifier creates an AppriseNotifier. If client is nil, or has no
// timeout configured (client.Timeout == 0, as on http.DefaultClient), a
// client with a defaultAppriseTimeout timeout is used instead so a hung
// Apprise endpoint cannot block Dispatcher.Dispatch indefinitely.
func NewAppriseNotifier(cfg AppriseConfig, client *http.Client) *AppriseNotifier {
	if client == nil || client.Timeout == 0 {
		client = &http.Client{Timeout: defaultAppriseTimeout}
	}
	return &AppriseNotifier{cfg: cfg, client: client}
}

// Name returns the notifier identifier.
func (a *AppriseNotifier) Name() string { return "apprise" }

// Config returns a copy of the notifier's configuration.
func (a *AppriseNotifier) Config() AppriseConfig { return a.cfg }

// Accepts reports whether this notifier is configured to handle t.
func (a *AppriseNotifier) Accepts(t EventType) bool {
	return acceptsAny(a.cfg.EventMask, t)
}

// Send posts a JSON notification to the configured Apprise API.
func (a *AppriseNotifier) Send(ctx context.Context, e Event) error {
	body := map[string]string{
		"urls":  a.cfg.ServiceURL,
		"title": e.Title,
		"body":  e.Body,
		"type":  appriseType(e.Type),
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("apprise: marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.URL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("apprise: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("apprise: http post: %w", err)
	}
	defer func() {
		// Drain the body so the underlying TCP connection can be reused by
		// the http.Client's connection pool. Without draining, the transport
		// must close the connection.
		//nolint:errcheck // body drain and close are best-effort cleanup
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("apprise: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// appriseType maps an EventType to the Apprise notification type string.
func appriseType(t EventType) string {
	switch t {
	case DownloadComplete, PostProcessingComplete, QueueDone:
		return "success"
	case DownloadFailed, PostProcessingFailed, Error:
		return "failure"
	case DiskFull, Warning:
		return "warning"
	default:
		return "info"
	}
}
