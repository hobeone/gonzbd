package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

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

// NewAppriseNotifier creates an AppriseNotifier. If client is nil, http.DefaultClient is used.
func NewAppriseNotifier(cfg AppriseConfig, client *http.Client) *AppriseNotifier {
	if client == nil {
		client = http.DefaultClient
	}
	return &AppriseNotifier{cfg: cfg, client: client}
}

// Name returns the notifier identifier.
func (a *AppriseNotifier) Name() string { return "apprise" }

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
