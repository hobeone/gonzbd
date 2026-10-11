package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// inFlightResidency is a Residency whose Hydrate waits for a hydration the
// test holds in flight, as the application's does, and reports each call on
// entered.
type inFlightResidency struct {
	mu      sync.Mutex
	busy    chan struct{} // closed when the held hydration finishes; nil when none
	entered chan struct{}
}

func (r *inFlightResidency) Hydrate(ctx context.Context, _ string) error {
	r.mu.Lock()
	busy := r.busy
	r.mu.Unlock()
	select {
	case r.entered <- struct{}{}:
	default:
	}
	if busy == nil {
		return nil
	}
	select {
	case <-busy:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *inFlightResidency) Evict(string) {}

// TestSetName_WaitsForAHydrationInstallingFilenames pins the hydration side of
// #838: a hydration attaches a restored job's content before it installs the
// filenames it verified, so in between the job has progress and no filename.
// A rename in that window must wait for the install and then be refused,
// rather than read DownloadBegun from the half-installed record.
func TestSetName_WaitsForAHydrationInstallingFilenames(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{{ID: "a", SortKey: 1, Header: Header{Name: "a"}, Intent: job.IntentPause}})
	res := &inFlightResidency{entered: make(chan struct{}, 1)}
	d := newTestDispatcher(t, withStore(st), withResidency(res))
	if err := d.restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	j, ok := d.Job("a")
	if !ok {
		t.Fatal("job a is not registered")
	}

	// A hydration in flight, between its AttachContent and its install.
	busy := make(chan struct{})
	res.mu.Lock()
	res.busy = busy
	res.mu.Unlock()
	m := job.NewManifest([]job.JobFile{{Subject: "f", Bytes: 100,
		Articles: []job.JobArticle{{ID: "a1", Bytes: 100, Number: 1}}}})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- d.SetName("a", "renamed") }()
	select {
	case err := <-done:
		t.Fatalf("SetName = %v while a hydration had attached content but not installed its filenames; it did not wait for the hydration", err)
	case <-res.entered:
	}

	if _, err := j.InstallFileVerification(job.FileVerification{FileIdx: 0, Filename: "f"}); err != nil {
		t.Fatalf("InstallFileVerification: %v", err)
	}
	close(busy)
	if err := <-done; !errors.Is(err, ErrJobStarted) {
		t.Errorf("SetName after the hydration installed a filename = %v, want ErrJobStarted", err)
	}
	if got := j.Name(); got != "a" {
		t.Errorf("a refused rename changed the name to %q", got)
	}
}

// TestLoadProgress_RecordsOnlyALoadItMade pins that LoadProgress records a
// load (markResident) for a job it hydrated, whether it had no progress or was
// evicted with progress, and not for one already resident:
// a freshly added job holds its content without a recorded load, and a rename
// that only waited on Hydrate must not make it a candidate for the tick's
// eviction.
func TestLoadProgress_RecordsOnlyALoadItMade(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("fresh", "fresh", job.PolicyFromPP(3))
	m := job.NewManifest([]job.JobFile{{Subject: "f", Bytes: 100,
		Articles: []job.JobArticle{{ID: "a1", Bytes: 100, Number: 1}}}})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if err := d.Add(context.Background(), j, Header{Name: "fresh"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.LoadProgress(context.Background(), "fresh"); err != nil {
		t.Fatalf("LoadProgress: %v", err)
	}
	if d.isResident("fresh") {
		t.Error("LoadProgress recorded a load for a job that was already resident")
	}

	st := &fakeStore{}
	st.seed([]Persisted{{ID: "restored", SortKey: 1, Header: Header{Name: "restored"}, Intent: job.IntentPause}})
	d2 := newTestDispatcher(t, withStore(st))
	if err := d2.restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := d2.LoadProgress(context.Background(), "restored"); err != nil {
		t.Fatalf("LoadProgress: %v", err)
	}
	if !d2.isResident("restored") {
		t.Error("LoadProgress did not record the load it made")
	}

	// An evicted job keeps its progress: LoadProgress reloads its manifest
	// through Hydrate and records that load too.
	fr := &fakeResidency{}
	d3 := newTestDispatcher(t, withResidency(fr))
	ev := job.New("evicted", "evicted", job.PolicyFromPP(3))
	if err := ev.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if err := d3.Add(context.Background(), ev, Header{Name: "evicted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	ev.Evict()
	if !ev.HasProgress() || ev.Resident() {
		t.Fatal("the evicted job does not have progress without a manifest")
	}
	fr.onHydrate = func(string) {
		if err := ev.RestoreContent(m); err != nil {
			t.Errorf("RestoreContent: %v", err)
		}
	}
	if err := d3.LoadProgress(context.Background(), "evicted"); err != nil {
		t.Fatalf("LoadProgress: %v", err)
	}
	if !ev.Resident() {
		t.Error("LoadProgress did not reload an evicted job's manifest")
	}
	if !d3.isResident("evicted") {
		t.Error("LoadProgress did not record the load it made for an evicted job")
	}
}
