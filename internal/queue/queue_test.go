package queue

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/constants"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
)

// makeParsed builds a minimal nzb.NZB suitable for NewJob.
func makeParsed(t *testing.T, nFiles int) *nzb.NZB {
	t.Helper()
	parsed := &nzb.NZB{
		Meta:   map[string][]string{"title": {"test"}},
		Groups: []string{"alt.binaries.test"},
		AvgAge: time.Unix(1700000000, 0),
	}
	for i := 0; i < nFiles; i++ {
		parsed.Files = append(parsed.Files, nzb.File{
			Subject: "file.bin",
			Date:    time.Unix(1700000000, 0),
			Bytes:   1_000_000,
			Articles: []nzb.Article{
				{ID: "a1@h", Bytes: 500_000, Number: 1},
				{ID: "a2@h", Bytes: 500_000, Number: 2},
			},
		})
	}
	return parsed
}

func makeJob(t *testing.T, name string, pri constants.Priority) *Job {
	t.Helper()
	parsed := makeParsed(t, 1)
	job, err := NewJob(parsed, AddOptions{
		Filename: name + ".nzb",
		Priority: pri,
	})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	return job
}

func TestNewJobDerivesName(t *testing.T) {
	tests := []struct {
		filename string
		want     string
	}{
		{"show.nzb", "show"},
		{"/watch/My.Show.S01E02.nzb", "My.Show.S01E02"},
		{"archive.nzb.gz", "archive"},
		{"archive.nzb.bz2", "archive"},
		{"noext", "noext"},
	}
	for _, tc := range tests {
		t.Run(tc.filename, func(t *testing.T) {
			j, err := NewJob(makeParsed(t, 1), AddOptions{Filename: tc.filename})
			if err != nil {
				t.Fatalf("NewJob: %v", err)
			}
			if j.Name != tc.want {
				t.Errorf("Name = %q, want %q", j.Name, tc.want)
			}
		})
	}
}

func TestNewJobAssignsUniqueID(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 100; i++ {
		j, err := NewJob(makeParsed(t, 1), AddOptions{Filename: "f.nzb"})
		if err != nil {
			t.Fatalf("NewJob: %v", err)
		}
		if len(j.ID) != 16 {
			t.Fatalf("ID length = %d, want 16", len(j.ID))
		}
		if _, dup := seen[j.ID]; dup {
			t.Fatalf("duplicate ID generated: %s", j.ID)
		}
		seen[j.ID] = struct{}{}
	}
}

func TestNewJobCopiesArticleState(t *testing.T) {
	parsed := makeParsed(t, 2)
	j, err := NewJob(parsed, AddOptions{Filename: "f.nzb"})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if len(j.Files) != 2 {
		t.Fatalf("len(Files) = %d, want 2", len(j.Files))
	}
	if j.TotalBytes != 2_000_000 || j.RemainingBytes != 2_000_000 {
		t.Errorf("bytes = (%d, %d), want both 2000000", j.TotalBytes, j.RemainingBytes)
	}
	if j.Status != constants.StatusQueued {
		t.Errorf("Status = %q, want Queued", j.Status)
	}
	// Mutating the job must not leak into the parser output.
	j.Files[0].Articles[0].Done = true
	if parsed.Files[0].Articles[0].Bytes != 500_000 {
		t.Errorf("parser article mutated by job update")
	}
}

func TestAddInsertsInPriorityOrder(t *testing.T) {
	q := New()
	// Add in scrambled order.
	low := makeJob(t, "low", constants.LowPriority)
	high := makeJob(t, "high", constants.HighPriority)
	normal := makeJob(t, "normal", constants.NormalPriority)
	for _, j := range []*Job{low, high, normal} {
		if err := q.Add(j); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	got := q.List()
	want := []*Job{high, normal, low}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Fatalf("position %d: got %s, want %s (full: %v)",
				i, got[i].ID, want[i].ID, ids(got))
		}
	}
}

func TestAddWithinTierIsFIFO(t *testing.T) {
	q := New()
	a := makeJob(t, "a", constants.NormalPriority)
	b := makeJob(t, "b", constants.NormalPriority)
	c := makeJob(t, "c", constants.NormalPriority)
	for _, j := range []*Job{a, b, c} {
		if err := q.Add(j); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	got := ids(q.List())
	want := []string{a.ID, b.ID, c.ID}
	if !equalSlice(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestAddDuplicateIDFails(t *testing.T) {
	q := New()
	j := makeJob(t, "j", constants.NormalPriority)
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}
	err := q.Add(j)
	if err == nil || !strings.Contains(err.Error(), "already present") {
		t.Errorf("duplicate Add error = %v, want 'already present'", err)
	}
}

func TestRemove(t *testing.T) {
	q := New()
	a := makeJob(t, "a", constants.NormalPriority)
	b := makeJob(t, "b", constants.NormalPriority)
	_ = q.Add(a)
	_ = q.Add(b)

	if err := q.Remove(a.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("Len = %d, want 1", q.Len())
	}
	if _, err := q.Get(a.ID); err == nil {
		t.Errorf("Get after Remove should fail")
	}

	err := q.Remove("nonexistent")
	if err == nil {
		t.Errorf("Remove(unknown) should error")
	}
}

func TestPauseResumePerJob(t *testing.T) {
	q := New()
	j := makeJob(t, "j", constants.NormalPriority)
	_ = q.Add(j)
	// Drain the add signal before asserting on Resume signal.
	<-q.Notify()

	if err := q.Pause(j.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	got, _ := q.Get(j.ID)
	if got.Status != constants.StatusPaused {
		t.Errorf("Status after Pause = %q, want Paused", got.Status)
	}

	if err := q.Resume(j.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, _ = q.Get(j.ID)
	if got.Status != constants.StatusQueued {
		t.Errorf("Status after Resume = %q, want Queued", got.Status)
	}

	// Resume must signal the downloader.
	select {
	case <-q.Notify():
	case <-time.After(time.Second):
		t.Errorf("Resume did not signal notify channel")
	}
}

func TestPauseAllResumeAll(t *testing.T) {
	q := New()
	if q.IsPaused() {
		t.Error("new queue should not be paused")
	}
	q.PauseAll()
	if !q.IsPaused() {
		t.Error("PauseAll did not set paused")
	}
	// Drain any stale notifications.
	select {
	case <-q.Notify():
	default:
	}
	q.ResumeAll()
	if q.IsPaused() {
		t.Error("ResumeAll did not clear paused")
	}
	select {
	case <-q.Notify():
	case <-time.After(time.Second):
		t.Error("ResumeAll did not signal")
	}
}

func TestReorder(t *testing.T) {
	q := New()
	a := makeJob(t, "a", constants.NormalPriority)
	b := makeJob(t, "b", constants.NormalPriority)
	c := makeJob(t, "c", constants.NormalPriority)
	for _, j := range []*Job{a, b, c} {
		_ = q.Add(j)
	}

	// Move c to position 0.
	if err := q.Reorder(c.ID, 0); err != nil {
		t.Fatalf("Reorder: %v", err)
	}
	got := ids(q.List())
	want := []string{c.ID, a.ID, b.ID}
	if !equalSlice(got, want) {
		t.Errorf("after Reorder to 0: %v, want %v", got, want)
	}

	// Clamp: newIndex too large goes to end.
	if err := q.Reorder(c.ID, 999); err != nil {
		t.Fatalf("Reorder clamp: %v", err)
	}
	got = ids(q.List())
	want = []string{a.ID, b.ID, c.ID}
	if !equalSlice(got, want) {
		t.Errorf("after clamp Reorder: %v, want %v", got, want)
	}

	if err := q.Reorder("nonexistent", 0); err == nil {
		t.Errorf("Reorder(unknown) should error")
	}
}

func TestMarkFileComplete(t *testing.T) {
	q := New()
	j := makeJob(t, "j", constants.NormalPriority)
	_ = q.Add(j)

	if err := q.MarkFileComplete(j.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}

	got, _ := q.Get(j.ID)
	if !got.Files[0].Complete {
		t.Error("File was not marked complete")
	}

	// Invalid index
	if err := q.MarkFileComplete(j.ID, 99); err == nil {
		t.Error("MarkFileComplete(99) should error")
	}
}

func TestMarkArticleFailed(t *testing.T) {
	q := New()
	j := makeJob(t, "j", constants.NormalPriority)
	_ = q.Add(j)

	msgID := j.Files[0].Articles[0].ID
	initialRemaining := j.RemainingBytes

	first, err := q.MarkArticleFailed(j.ID, msgID)
	if err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	if !first {
		t.Error("expected first=true")
	}

	got, _ := q.Get(j.ID)
	if !got.Files[0].Articles[0].Done {
		t.Error("article should be marked Done")
	}
	if got.RemainingBytes != initialRemaining {
		t.Errorf("RemainingBytes changed: got %d, want %d", got.RemainingBytes, initialRemaining)
	}

	// Repeat failure should return false
	first, _ = q.MarkArticleFailed(j.ID, msgID)
	if first {
		t.Error("expected first=false on repeat")
	}
}

func TestNotifyCoalesces(t *testing.T) {
	q := New()
	for i := 0; i < 5; i++ {
		_ = q.Add(makeJob(t, "j", constants.NormalPriority))
	}
	// Five Adds, one buffered signal.
	n := 0
loop:
	for {
		select {
		case <-q.Notify():
			n++
		default:
			break loop
		}
	}
	if n != 1 {
		t.Errorf("drained %d signals from cap-1 channel, want 1", n)
	}
}

func TestNotifyFiresOnAdd(t *testing.T) {
	q := New()
	done := make(chan struct{})
	go func() {
		<-q.Notify()
		close(done)
	}()
	_ = q.Add(makeJob(t, "j", constants.NormalPriority))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Add did not signal notify channel")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()

	original := New()
	original.PauseAll()
	a := makeJob(t, "a", constants.HighPriority)
	b := makeJob(t, "b", constants.NormalPriority)
	c := makeJob(t, "c", constants.LowPriority)
	for _, j := range []*Job{a, b, c} {
		_ = original.Add(j)
	}
	// Mutate a runtime field to verify it round-trips.
	a.Files[0].Articles[0].Done = true
	a.RemainingBytes = 500_000

	if err := original.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.IsPaused() {
		t.Error("paused flag not restored")
	}
	if loaded.Len() != 3 {
		t.Fatalf("Len = %d, want 3", loaded.Len())
	}

	wantOrder := []string{a.ID, b.ID, c.ID}
	gotOrder := ids(loaded.List())
	if !equalSlice(gotOrder, wantOrder) {
		t.Errorf("order after load: %v, want %v", gotOrder, wantOrder)
	}

	restored, _ := loaded.Get(a.ID)
	if !restored.Files[0].Articles[0].Done {
		t.Error("article Done not round-tripped")
	}
	if restored.RemainingBytes != 500_000 {
		t.Errorf("RemainingBytes = %d, want 500000", restored.RemainingBytes)
	}
}

func TestLoadMissingReturnsEmptyQueue(t *testing.T) {
	dir := t.TempDir()
	q, err := Load(dir)
	if err != nil {
		t.Fatalf("Load on empty dir: %v", err)
	}
	if q.Len() != 0 {
		t.Errorf("Len = %d, want 0", q.Len())
	}
}

func TestSaveAtomicReplacesIndex(t *testing.T) {
	dir := t.TempDir()
	q := New()
	_ = q.Add(makeJob(t, "a", constants.NormalPriority))
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(dir, "queue.json.gz"))
	if err != nil {
		t.Fatalf("read first: %v", err)
	}

	// Add another job and save again; the index must change.
	_ = q.Add(makeJob(t, "b", constants.NormalPriority))
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(dir, "queue.json.gz"))
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Error("second save produced identical bytes")
	}

	// No leftover temp files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestLoadRejectsFutureVersion(t *testing.T) {
	dir := t.TempDir()
	// Hand-craft an index with an unsupported version.
	if err := writeGzJSON(filepath.Join(dir, "queue.json.gz"), &indexFile{
		Version: 999,
	}); err != nil {
		t.Fatalf("seed bad index: %v", err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("Load future-version error = %v, want version error", err)
	}
}

// TestConcurrentAddRemove drives many goroutines hitting the queue
// simultaneously. Run under -race to catch any missed locking.
func TestConcurrentAddRemove(t *testing.T) {
	q := New()
	const workers = 16
	const perWorker = 25

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			jobs := make([]*Job, 0, perWorker)
			for i := 0; i < perWorker; i++ {
				j := makeJob(t, "x", constants.NormalPriority)
				if err := q.Add(j); err != nil {
					t.Errorf("Add: %v", err)
					return
				}
				jobs = append(jobs, j)
			}
			for _, j := range jobs {
				// Interleave reads.
				_ = q.List()
				_ = q.Len()
				if _, err := q.Get(j.ID); err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				if err := q.Remove(j.ID); err != nil {
					t.Errorf("Remove: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if q.Len() != 0 {
		t.Errorf("Len after churn = %d, want 0", q.Len())
	}
}

func ids(js []*Job) []string {
	out := make([]string, len(js))
	for i, j := range js {
		out[i] = j.ID
	}
	return out
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
