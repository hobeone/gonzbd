package app

import (
	"log/slog"
	"sync"
	"testing"
)

// serverControlDownloader records the server-control calls the application
// delegates to its downloader.
type serverControlDownloader struct {
	*fakeDownloader
	mu          sync.Mutex
	disconnects int
	unblocked   []string
}

func (d *serverControlDownloader) DisconnectAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disconnects++
}

func (d *serverControlDownloader) UnblockServer(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.unblocked = append(d.unblocked, name)
	return name == "known"
}

// TestServerControls_DelegateToTheDownloader pins DisconnectAll and
// UnblockServer to the downloader New installed: each call reaches it once,
// and UnblockServer reports the downloader's answer for the named server.
func TestServerControls_DelegateToTheDownloader(t *testing.T) {
	t.Parallel()
	dl := &serverControlDownloader{fakeDownloader: newFakeDownloader()}
	app := newTestApplication(t, WithLogger(slog.New(slog.DiscardHandler)), WithDownloader(dl))

	app.DisconnectAll()
	if !app.UnblockServer("known") {
		t.Error(`UnblockServer("known") = false, want the downloader's true`)
	}
	if app.UnblockServer("other") {
		t.Error(`UnblockServer("other") = true, want the downloader's false`)
	}

	dl.mu.Lock()
	defer dl.mu.Unlock()
	if dl.disconnects != 1 {
		t.Errorf("DisconnectAll reached the downloader %d times, want 1", dl.disconnects)
	}
	if len(dl.unblocked) != 2 || dl.unblocked[0] != "known" || dl.unblocked[1] != "other" {
		t.Errorf("UnblockServer reached the downloader with %q, want [known other]", dl.unblocked)
	}
}
