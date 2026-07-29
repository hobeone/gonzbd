package queue

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

// makeParsed builds a minimal nzb.NZB suitable for NewJob.
func makeParsed(t *testing.T, nFiles int) *nzb.NZB {
	t.Helper()
	parsed := &nzb.NZB{
		Meta:   map[string][]string{"title": {"test"}},
		Groups: []string{"alt.binaries.test"},
		AvgAge: time.Unix(1700000000, 0),
	}
	for range nFiles {
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
	}, fsutil.SanitizeOptions{})
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
			j, err := NewJob(makeParsed(t, 1), AddOptions{Filename: tc.filename}, fsutil.SanitizeOptions{})
			if err != nil {
				t.Fatalf("NewJob: %v", err)
			}
			if j.Name != tc.want {
				t.Errorf("Name = %q, want %q", j.Name, tc.want)
			}
		})
	}
}

// TestNewJobStripsNZBFromExplicitName verifies that .nzb extensions are
// stripped from explicitly provided Name values (e.g. Sonarr's nzbname
// parameter). Without this, download directories end up named "movie.nzb/".
func TestNewJobStripsNZBFromExplicitName(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"My.Movie.2024.nzb", "My.Movie.2024"},
		{"My.Movie.2024.NZB", "My.Movie.2024"},
		{"My.Movie.2024.nzb.gz", "My.Movie.2024"},
		{"My.Movie.2024.nzb.bz2", "My.Movie.2024"},
		{"My.Movie.2024", "My.Movie.2024"},         // no extension — unchanged
		{"My.Movie.2024.mkv", "My.Movie.2024.mkv"}, // non-nzb ext — unchanged
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j, err := NewJob(makeParsed(t, 1), AddOptions{
				Name:     tc.name,
				Filename: "irrelevant.nzb",
			}, fsutil.SanitizeOptions{})
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
	for range 100 {
		j, err := NewJob(makeParsed(t, 1), AddOptions{Filename: "f.nzb"}, fsutil.SanitizeOptions{})
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
	j, err := NewJob(parsed, AddOptions{Filename: "f.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if j.Manifest().NumFiles() != 2 {
		t.Fatalf("NumFiles() = %d, want 2", j.Manifest().NumFiles())
	}
	if j.Manifest().TotalBytes() != 2_000_000 || j.Progress().RemainingBytes() != 2_000_000 {
		t.Errorf("bytes = (%d, %d), want both 2000000", j.Manifest().TotalBytes(), j.Progress().RemainingBytes())
	}
	if j.Status != constants.StatusQueued {
		t.Errorf("Status = %q, want Queued", j.Status)
	}
	// Mutating the job must not leak into the parser output.
	j.progress.done[0] = true
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

func TestQueue_HasDownloadingAndPostProcJobs(t *testing.T) {
	q := New()
	if q.HasDownloadingJobs() {
		t.Error("expected HasDownloadingJobs=false for empty queue")
	}
	if q.HasPostProcJobs() {
		t.Error("expected HasPostProcJobs=false for empty queue")
	}

	job1 := makeJob(t, "job1", constants.NormalPriority)
	if err := q.Add(job1); err != nil {
		t.Fatalf("Add job1: %v", err)
	}

	if !q.HasDownloadingJobs() {
		t.Error("expected HasDownloadingJobs=true for active job")
	}
	if q.HasPostProcJobs() {
		t.Error("expected HasPostProcJobs=false for downloading job")
	}

	// Move job1 to post-processing
	job1.PostProc = true
	if q.HasDownloadingJobs() {
		t.Error("expected HasDownloadingJobs=false when only post-proc job exists")
	}
	if !q.HasPostProcJobs() {
		t.Error("expected HasPostProcJobs=true when post-proc job exists")
	}

	// Pause queue
	q.PauseAll()
	if q.HasDownloadingJobs() {
		t.Error("expected HasDownloadingJobs=false when queue is paused")
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

	if err := q.Remove("nonexistent"); err == nil {
		t.Errorf("Remove(unknown) should error")
	}
}

// TestRemove_NilZerosSlotForGC verifies that removeAtLocked nil-zeroes the
// vacated capacity slot, allowing the GC to collect the removed *Job.
// Without this, the pointer remains reachable inside the slice's backing
// array even after the slice is shortened.
func TestRemove_NilZerosSlotForGC(t *testing.T) {
	q := New()
	a := makeJob(t, "a", constants.NormalPriority)
	b := makeJob(t, "b", constants.NormalPriority)
	c := makeJob(t, "c", constants.NormalPriority)
	_ = q.Add(a)
	_ = q.Add(b)
	_ = q.Add(c)

	// Remove the middle element.
	if err := q.Remove(b.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Peek inside the internal slice: the slot at len must be nil.
	q.mu.RLock()
	defer q.mu.RUnlock()
	jobs := q.jobs
	slack := jobs[:cap(jobs)]
	if slack[len(jobs)] != nil {
		t.Error("vacated slot beyond len is not nil — GC cannot collect removed *Job")
	}
}

func TestSetStatusEnforcesStateMachine(t *testing.T) {
	q := New()
	j := makeJob(t, "j", constants.NormalPriority)
	_ = q.Add(j) // Queued

	// Legal: Queued -> Downloading.
	if err := q.SetStatus(j.ID, constants.StatusDownloading); err != nil {
		t.Fatalf("legal transition rejected: %v", err)
	}

	// Illegal: Downloading -> Completed.
	if err := q.SetStatus(j.ID, constants.StatusCompleted); !errors.Is(err, ErrIllegalStatusTransition) {
		t.Fatalf("illegal transition err = %v, want ErrIllegalStatusTransition", err)
	}

	got, _ := q.Get(j.ID)
	if got.Status != constants.StatusDownloading {
		t.Errorf("status changed despite illegal transition: %q", got.Status)
	}

	if _, err := q.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v", err)
	}
}

func TestPauseResumePerJob(t *testing.T) {
	q := New()
	q.PauseAll()
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
	q.ResumeAll(context.Background())
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
	if !got.Progress().FileComplete(0) {
		t.Error("File was not marked complete")
	}

	// Invalid index
	if err := q.MarkFileComplete(j.ID, j.Manifest().NumFiles()); err == nil {
		t.Error("MarkFileComplete(NumFiles()) should error")
	}
}

func TestMarkArticleFailed(t *testing.T) {
	q := New()
	j := makeJob(t, "j", constants.NormalPriority)
	_ = q.Add(j)

	msgID := j.Manifest().ArticleID(0)
	initialRemaining := j.Progress().RemainingBytes()

	first, err := q.MarkArticleFailed(j.ID, msgID)
	if err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	if !first {
		t.Error("expected first=true")
	}

	got, _ := q.Get(j.ID)
	if !got.Progress().ArticleDone(0) {
		t.Error("article should be marked Done")
	}
	wantRemaining := initialRemaining - int64(j.Manifest().ArticleBytes(0))
	if got.Progress().RemainingBytes() != wantRemaining {
		t.Errorf("RemainingBytes mismatch: got %d, want %d", got.Progress().RemainingBytes(), wantRemaining)
	}

	// Repeat failure should return false
	first, _ = q.MarkArticleFailed(j.ID, msgID)
	if first {
		t.Error("expected first=false on repeat")
	}
}

// TestMarkArticleFailed_ParityWithBatched verifies that the singular
// MarkArticleFailed produces identical queue state to MarkArticlesFailed with
// a one-element slice. This guards against the two implementations drifting
// apart — historically they have diverged (see CLAUDE.md lessons learned).
func TestMarkArticleFailed_ParityWithBatched(t *testing.T) {
	// Build two identical queues and apply singular vs batched form to each.
	buildQ := func(t *testing.T) (*Queue, string, string) {
		t.Helper()
		q := New()
		j := makeJob(t, "j", constants.NormalPriority)
		_ = q.Add(j)
		msgID := j.Manifest().ArticleID(0)
		return q, j.ID, msgID
	}

	q1, jid1, mid1 := buildQ(t)
	q2, jid2, mid2 := buildQ(t)

	// Apply singular form to q1.
	first1, err1 := q1.MarkArticleFailed(jid1, mid1)

	// Apply batched form to q2 (one element).
	ft, err2 := q2.MarkArticlesFailed(jid2, []string{mid2})
	first2 := len(ft) > 0

	if err1 != nil || err2 != nil {
		t.Errorf("expected both forms to succeed: singular=%v batched=%v", err1, err2)
	}
	if first1 != first2 {
		t.Errorf("first-time flag mismatch: singular=%v batched=%v", first1, first2)
	}

	got1, _ := q1.Get(jid1)
	got2, _ := q2.Get(jid2)

	if got1.Progress().PendingArticles() != got2.Progress().PendingArticles() {
		t.Errorf("PendingArticles: singular=%d batched=%d", got1.Progress().PendingArticles(), got2.Progress().PendingArticles())
	}
	if got1.Progress().FailedBytes() != got2.Progress().FailedBytes() {
		t.Errorf("FailedBytes: singular=%d batched=%d", got1.Progress().FailedBytes(), got2.Progress().FailedBytes())
	}
	if got1.Progress().RemainingBytes() != got2.Progress().RemainingBytes() {
		t.Errorf("RemainingBytes: singular=%d batched=%d", got1.Progress().RemainingBytes(), got2.Progress().RemainingBytes())
	}
	if got1.Progress().ArticlesResolved() != got2.Progress().ArticlesResolved() {
		t.Errorf("ArticlesResolved: singular=%d batched=%d", got1.Progress().ArticlesResolved(), got2.Progress().ArticlesResolved())
	}
	if got1.Progress().ArticlesFailed() != got2.Progress().ArticlesFailed() {
		t.Errorf("ArticlesFailed: singular=%d batched=%d", got1.Progress().ArticlesFailed(), got2.Progress().ArticlesFailed())
	}
	done1, failed1 := got1.Progress().ArticleDone(0), got1.Progress().ArticleFailed(0)
	done2, failed2 := got2.Progress().ArticleDone(0), got2.Progress().ArticleFailed(0)
	if done1 != done2 || failed1 != failed2 {
		t.Errorf("article state: singular Done=%v Failed=%v, batched Done=%v Failed=%v",
			done1, failed1, done2, failed2)
	}
	if got1.Progress().FilePending(0) != got2.Progress().FilePending(0) {
		t.Errorf("Files[0].Pending: singular=%d batched=%d", got1.Progress().FilePending(0), got2.Progress().FilePending(0))
	}
}

func TestNotifyCoalesces(t *testing.T) {
	q := New()
	for range 5 {
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
	a.progress.done[0] = true
	a.progress.remainingBytes = 500_000

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
	if !restored.Progress().ArticleDone(0) {
		t.Error("article Done not round-tripped")
	}
	if restored.Progress().RemainingBytes() != 500_000 {
		t.Errorf("RemainingBytes = %d, want 500000", restored.Progress().RemainingBytes())
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
	for range workers {
		go func() {
			defer wg.Done()
			jobs := make([]*Job, 0, perWorker)
			for range perWorker {
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

// TestIsDirty verifies the dirty-flag lifecycle: fresh queue is clean,
// mutations set dirty, Save clears it.
func TestIsDirty(t *testing.T) {
	dir := t.TempDir()
	q := New()

	if q.IsDirty() {
		t.Fatal("new queue should not be dirty")
	}

	j := makeJob(t, "dirty-test", constants.NormalPriority)
	_ = q.Add(j)
	_ = q.Save(dir)

	// MarkArticleDone sets dirty.
	msgID := j.Manifest().ArticleID(0)
	if err := q.MarkArticleDone(j.ID, msgID); err != nil {
		t.Fatalf("MarkArticleDone: %v", err)
	}
	if !q.IsDirty() {
		t.Error("MarkArticleDone should set dirty")
	}

	// Save clears dirty.
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if q.IsDirty() {
		t.Error("Save should clear dirty")
	}

	// MarkArticleFailed sets dirty.
	msgID2 := j.Manifest().ArticleID(1)
	first, err := q.MarkArticleFailed(j.ID, msgID2)
	if err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	if !first {
		t.Fatal("expected first=true")
	}
	if !q.IsDirty() {
		t.Error("MarkArticleFailed should set dirty")
	}
	_ = q.Save(dir)

	// MarkFileComplete sets dirty.
	if err := q.MarkFileComplete(j.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	if !q.IsDirty() {
		t.Error("MarkFileComplete should set dirty")
	}
	_ = q.Save(dir)

	// MarkArticlesDone sets dirty.
	j2 := makeJob(t, "batch-done", constants.NormalPriority)
	_ = q.Add(j2)
	gotJ2, _ := q.Get(j2.ID)
	ids2 := []string{gotJ2.Manifest().ArticleID(0), gotJ2.Manifest().ArticleID(1)}
	if err := q.MarkArticlesDone(j2.ID, ids2); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	if !q.IsDirty() {
		t.Error("MarkArticlesDone should set dirty")
	}
	_ = q.Save(dir)

	// MarkArticlesFailed sets dirty.
	j3 := makeJob(t, "batch-fail", constants.NormalPriority)
	_ = q.Add(j3)
	gotJ3, _ := q.Get(j3.ID)
	ids3 := []string{gotJ3.Manifest().ArticleID(0)}
	if _, err := q.MarkArticlesFailed(j3.ID, ids3); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}
	if !q.IsDirty() {
		t.Error("MarkArticlesFailed should set dirty")
	}
}

func TestDirtyFlagOnMutations(t *testing.T) {
	dir := t.TempDir()
	q := New()
	q.PauseAll()

	// 1. Add
	j1 := makeJob(t, "job1", constants.NormalPriority)
	if err := q.Add(j1); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !q.IsDirty() {
		t.Error("Add should set dirty")
	}
	_ = q.Save(dir)

	// 2. Pause
	if err := q.Pause(j1.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if !q.IsDirty() {
		t.Error("Pause should set dirty")
	}
	_ = q.Save(dir)

	// 3. Resume
	if err := q.Resume(j1.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !q.IsDirty() {
		t.Error("Resume should set dirty")
	}
	_ = q.Save(dir)

	// 4. SetStatus
	if err := q.SetStatus(j1.ID, constants.StatusDownloading); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if !q.IsDirty() {
		t.Error("SetStatus should set dirty")
	}
	_ = q.Save(dir)

	// 5. Reorder
	j2 := makeJob(t, "job2", constants.NormalPriority)
	_ = q.Add(j2)
	_ = q.Save(dir)
	if err := q.Reorder(j1.ID, 1); err != nil {
		t.Fatalf("Reorder: %v", err)
	}
	if !q.IsDirty() {
		t.Error("Reorder should set dirty")
	}
	_ = q.Save(dir)

	// 6. PauseAll
	q.PauseAll()
	if !q.IsDirty() {
		t.Error("PauseAll should set dirty")
	}
	_ = q.Save(dir)

	// 7. ResumeAll
	q.ResumeAll(context.Background())
	if !q.IsDirty() {
		t.Error("ResumeAll should set dirty")
	}
	_ = q.Save(dir)

	// 8. Remove
	if err := q.Remove(j1.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !q.IsDirty() {
		t.Error("Remove should set dirty")
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

func TestSetPriority(t *testing.T) {
	q := New()
	jHigh := makeJob(t, "high", constants.HighPriority)
	jNorm := makeJob(t, "normal", constants.NormalPriority)
	jLow := makeJob(t, "low", constants.LowPriority)
	for _, j := range []*Job{jHigh, jNorm, jLow} {
		if err := q.Add(j); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	// Verify initial order: high, normal, low.
	got := ids(q.List())
	want := []string{jHigh.ID, jNorm.ID, jLow.ID}
	if !equalSlice(got, want) {
		t.Fatalf("initial order = %v, want %v", got, want)
	}

	// Clear dirty flag to verify SetPriority sets it.
	q.dirty.Store(false)

	// Promote low to high priority.
	if err := q.SetPriority(jLow.ID, constants.HighPriority); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	// Verify dirty flag was set.
	if !q.IsDirty() {
		t.Error("SetPriority should set dirty flag")
	}

	// Verify the job's Priority field was updated.
	updated, err := q.Get(jLow.ID)
	if err != nil {
		t.Fatalf("Get after SetPriority: %v", err)
	}
	if updated.Priority != constants.HighPriority {
		t.Errorf("Priority = %d, want %d (HighPriority)", updated.Priority, constants.HighPriority)
	}

	// Verify order: jHigh, jLow (now high, end of tier), jNorm.
	got = ids(q.List())
	want = []string{jHigh.ID, jLow.ID, jNorm.ID}
	if !equalSlice(got, want) {
		t.Errorf("order after promote = %v, want %v", got, want)
	}

	// Error on nonexistent ID.
	if err := q.SetPriority("nonexistent", constants.NormalPriority); err == nil {
		t.Error("SetPriority(unknown) should error")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want 'not found'", err)
	}
}

func TestSetPriority_InvalidPriority(t *testing.T) {
	q := New()
	j := makeJob(t, "pri-invalid", constants.NormalPriority)
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}
	for _, invalidPri := range []constants.Priority{-99, 5, 10} {
		err := q.SetPriority(j.ID, invalidPri)
		if err == nil {
			t.Errorf("SetPriority(%d) should error", invalidPri)
		} else if !strings.Contains(err.Error(), "invalid priority") {
			t.Errorf("SetPriority(%d) error = %v, want 'invalid priority'", invalidPri, err)
		}
	}
}

func TestSetPP(t *testing.T) {
	q := New()
	j := makeJob(t, "pp-test", constants.NormalPriority)
	j.PP = 1 // Start with +Repair.
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Clear dirty flag to verify SetPP sets it.
	q.dirty.Store(false)

	// Change PP to 3 (+Delete).
	if err := q.SetPP(j.ID, 3); err != nil {
		t.Fatalf("SetPP: %v", err)
	}

	if !q.IsDirty() {
		t.Error("SetPP should set dirty flag")
	}

	got, err := q.Get(j.ID)
	if err != nil {
		t.Fatalf("Get after SetPP: %v", err)
	}
	if got.PP != 3 {
		t.Errorf("PP = %d, want 3", got.PP)
	}

	// Error on nonexistent ID.
	if err := q.SetPP("nonexistent", 0); err == nil {
		t.Error("SetPP(unknown) should error")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want 'not found'", err)
	}
}

func TestSetPP_InvalidLevel(t *testing.T) {
	q := New()
	j := makeJob(t, "pp-invalid", constants.NormalPriority)
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}
	for _, invalidPP := range []int{-1, 4, 100} {
		err := q.SetPP(j.ID, invalidPP)
		if err == nil {
			t.Errorf("SetPP(%d) should error", invalidPP)
		} else if !strings.Contains(err.Error(), "invalid post-processing level") {
			t.Errorf("SetPP(%d) error = %v, want 'invalid post-processing level'", invalidPP, err)
		}
	}
}

func TestSetCategory(t *testing.T) {
	cats := []config.CategoryConfig{
		{Name: "Default", PP: 3, Script: "default.sh", Priority: int(constants.NormalPriority)},
		{Name: "movies", PP: 2, Script: "movies.sh", Priority: int(constants.HighPriority)},
		{Name: "tv", PP: 1, Script: "tv.sh", Priority: int(constants.LowPriority)},
	}

	t.Run("inherits PP and script from category", func(t *testing.T) {
		q := New()
		j := makeJob(t, "j", constants.NormalPriority)
		j.Category = "tv"
		j.PP = 1
		j.Script = "tv.sh"
		if err := q.Add(j); err != nil {
			t.Fatalf("Add: %v", err)
		}

		q.dirty.Store(false)
		if err := q.SetCategory(j.ID, "movies", cats); err != nil {
			t.Fatalf("SetCategory: %v", err)
		}

		got, err := q.Get(j.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Category != "movies" {
			t.Errorf("Category = %q, want %q", got.Category, "movies")
		}
		if got.PP != 2 {
			t.Errorf("PP = %d, want 2", got.PP)
		}
		if got.Script != "movies.sh" {
			t.Errorf("Script = %q, want %q", got.Script, "movies.sh")
		}
		if got.Priority != constants.HighPriority {
			t.Errorf("Priority = %d, want HighPriority (%d)", got.Priority, constants.HighPriority)
		}
		if !q.IsDirty() {
			t.Error("SetCategory should set dirty flag")
		}
	})

	t.Run("priority change re-slots job in queue", func(t *testing.T) {
		q := New()
		// Add three jobs: high, normal(target), low.
		jH := makeJob(t, "high", constants.HighPriority)
		jN := makeJob(t, "normal", constants.NormalPriority)
		jL := makeJob(t, "low", constants.LowPriority)
		jN.Category = "tv"
		for _, jj := range []*Job{jH, jN, jL} {
			if err := q.Add(jj); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
		// Initial order: high, normal, low.
		got := ids(q.List())
		want := []string{jH.ID, jN.ID, jL.ID}
		if !equalSlice(got, want) {
			t.Fatalf("initial order = %v, want %v", got, want)
		}

		// Switch jN from "tv"(Low) to "movies"(High) — should move it into the High tier.
		if err := q.SetCategory(jN.ID, "movies", cats); err != nil {
			t.Fatalf("SetCategory: %v", err)
		}
		got = ids(q.List())
		// jH was already High; jN (now High) is appended to end of High tier; jL remains Low.
		wantAfter := []string{jH.ID, jN.ID, jL.ID}
		if !equalSlice(got, wantAfter) {
			t.Errorf("order after SetCategory = %v, want %v", got, wantAfter)
		}
		// Verify priority was actually updated.
		updated, _ := q.Get(jN.ID)
		if updated.Priority != constants.HighPriority {
			t.Errorf("Priority after SetCategory = %d, want HighPriority", updated.Priority)
		}
	})

	t.Run("empty name falls back to Default", func(t *testing.T) {
		q := New()
		j := makeJob(t, "j", constants.NormalPriority)
		j.PP = 0
		if err := q.Add(j); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if err := q.SetCategory(j.ID, "", cats); err != nil {
			t.Fatalf("SetCategory with empty name: %v", err)
		}
		got, _ := q.Get(j.ID)
		// FindCategory("") returns the "Default" entry.
		if got.Category != "Default" {
			t.Errorf("Category = %q, want Default", got.Category)
		}
		if got.PP != 3 {
			t.Errorf("PP = %d, want 3 (from Default)", got.PP)
		}
	})

	t.Run("unknown name falls back to Default", func(t *testing.T) {
		q := New()
		j := makeJob(t, "j", constants.NormalPriority)
		if err := q.Add(j); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if err := q.SetCategory(j.ID, "nonexistent-cat", cats); err != nil {
			t.Fatalf("SetCategory: %v", err)
		}
		got, _ := q.Get(j.ID)
		if got.Category != "Default" {
			t.Errorf("Category = %q, want Default fallback", got.Category)
		}
	})

	t.Run("no priority re-slot when priority unchanged", func(t *testing.T) {
		// "Default" has NormalPriority; job starts at NormalPriority — no re-slot needed.
		q := New()
		jA := makeJob(t, "a", constants.NormalPriority)
		jB := makeJob(t, "b", constants.NormalPriority)
		if err := q.Add(jA); err != nil {
			t.Fatalf("Add jA: %v", err)
		}
		if err := q.Add(jB); err != nil {
			t.Fatalf("Add jB: %v", err)
		}
		// Switch jA to Default (also NormalPriority) — order must be unchanged.
		if err := q.SetCategory(jA.ID, "Default", cats); err != nil {
			t.Fatalf("SetCategory: %v", err)
		}
		got := ids(q.List())
		want := []string{jA.ID, jB.ID}
		if !equalSlice(got, want) {
			t.Errorf("order after same-priority SetCategory = %v, want %v", got, want)
		}
	})

	t.Run("ErrNotFound on missing job", func(t *testing.T) {
		q := New()
		err := q.SetCategory("nonexistent", "movies", cats)
		if err == nil {
			t.Fatal("expected error for missing job")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error = %v, want 'not found'", err)
		}
	})
}

func TestSetName(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeJob(t, "original", constants.NormalPriority)
	_ = q.Add(j)

	if err := q.SetName(j.ID, "new-name"); err != nil {
		t.Fatalf("SetName: %v", err)
	}
	got, _ := q.Get(j.ID)
	if got.Name != "new-name" {
		t.Errorf("Name = %q, want %q", got.Name, "new-name")
	}
	if !q.IsDirty() {
		t.Error("SetName should set dirty")
	}
	if err := q.SetName("nonexistent", "x"); err == nil {
		t.Error("SetName(unknown) should error")
	}
}

func TestSetName_Sanitization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		sOpts     fsutil.SanitizeOptions
		inputName string
		wantName  string
	}{
		{
			name:      "path traversal",
			inputName: "../../etc/passwd",
			wantName:  ".._.._etc_passwd",
		},
		{
			name:      "absolute unix path",
			inputName: "/tmp/malicious/job",
			wantName:  "_tmp_malicious_job",
		},
		{
			name:      "absolute windows path",
			inputName: `C:\Windows\System32`,
			wantName:  "C__Windows_System32",
		},
		{
			name:      "strip nzb extension",
			inputName: "My.Movie.nzb",
			wantName:  "My.Movie",
		},
		{
			name:      "empty name fallback",
			inputName: "",
			wantName:  "unknown",
		},
		{
			name:      "all illegal characters fallback",
			inputName: "///",
			wantName:  "___",
		},
		{
			name: "custom options regex and illegal replacement",
			sOpts: fsutil.SanitizeOptions{
				CleanupRegexps:     fsutil.CompileCleanupList([]string{"^SPAM-"}),
				ReplaceIllegalWith: "-",
			},
			inputName: "SPAM-Movie/Name.nzb",
			wantName:  "Movie-Name",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := New(WithSanitizeOptions(tc.sOpts))
			j := makeJob(t, "orig", constants.NormalPriority)
			if err := q.Add(j); err != nil {
				t.Fatalf("Add: %v", err)
			}
			if err := q.SetName(j.ID, tc.inputName); err != nil {
				t.Fatalf("SetName(%q): %v", tc.inputName, err)
			}
			got, err := q.Get(j.ID)
			if err != nil {
				t.Fatalf("Get(%s) failed: %v", j.ID, err)
			}
			if got.Name != tc.wantName {
				t.Errorf("SetName(%q) resulting Name = %q, want %q", tc.inputName, got.Name, tc.wantName)
			}
		})
	}
}

func TestSetSanitizeOptions(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeJob(t, "orig", constants.NormalPriority)
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Update options at runtime
	q.SetSanitizeOptions(fsutil.SanitizeOptions{
		CleanupRegexps:     fsutil.CompileCleanupList([]string{"^PREFIX-"}),
		ReplaceIllegalWith: "-",
	})

	if err := q.SetName(j.ID, "PREFIX-Movie/Name.nzb"); err != nil {
		t.Fatalf("SetName: %v", err)
	}
	got, err := q.Get(j.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "Movie-Name" {
		t.Errorf("Name = %q, want %q", got.Name, "Movie-Name")
	}
}

func TestSetScript(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeJob(t, "script-test", constants.NormalPriority)
	_ = q.Add(j)

	if err := q.SetScript(j.ID, "my-script.sh"); err != nil {
		t.Fatalf("SetScript: %v", err)
	}
	got, _ := q.Get(j.ID)
	if got.Script != "my-script.sh" {
		t.Errorf("Script = %q, want %q", got.Script, "my-script.sh")
	}
	if !q.IsDirty() {
		t.Error("SetScript should set dirty")
	}
	// Empty script clears the field.
	if err := q.SetScript(j.ID, ""); err != nil {
		t.Fatalf("SetScript empty: %v", err)
	}
	got, _ = q.Get(j.ID)
	if got.Script != "" {
		t.Errorf("Script = %q, want empty", got.Script)
	}
	if err := q.SetScript("nonexistent", "x"); err == nil {
		t.Error("SetScript(unknown) should error")
	}
}

func TestSetPar2ReleaseReason(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeJob(t, "par2-reason-test", constants.NormalPriority)
	_ = q.Add(j)

	if err := q.SetPar2ReleaseReason(j.ID, "damaged"); err != nil {
		t.Fatalf("SetPar2ReleaseReason: %v", err)
	}
	got, _ := q.Get(j.ID)
	if got.Progress().Par2ReleaseReason() != "damaged" {
		t.Errorf("Par2ReleaseReason = %q, want %q", got.Progress().Par2ReleaseReason(), "damaged")
	}
	if !q.IsDirty() {
		t.Error("SetPar2ReleaseReason should set dirty")
	}
	if err := q.SetPar2ReleaseReason("nonexistent", "x"); err == nil {
		t.Error("SetPar2ReleaseReason(unknown) should error")
	}
}

func TestQueueUnexportedHelpersDirect(t *testing.T) {
	t.Parallel()

	t.Run("indexOfLocked and removeAtLocked", func(t *testing.T) {
		q := New()
		j1 := &Job{ID: "j1", Priority: constants.NormalPriority}
		j1.manifest = newManifest(nil)
		j1.progress = newJobProgress(j1.manifest)
		j2 := &Job{ID: "j2", Priority: constants.NormalPriority}
		j2.manifest = newManifest(nil)
		j2.progress = newJobProgress(j2.manifest)
		_ = q.Add(j1)
		_ = q.Add(j2)

		q.mu.Lock()
		defer q.mu.Unlock()

		idx, ok := q.indexOfLocked("j2")
		if !ok || idx != 1 {
			t.Errorf("indexOfLocked(j2) = %d, %t, want 1, true", idx, ok)
		}

		q.removeAtLocked(1)
		if len(q.jobs) != 1 || q.jobs[0].ID != "j1" {
			t.Errorf("after removeAtLocked(1) jobs: %v", q.jobs)
		}

		_, ok = q.indexOfLocked("j2")
		if ok {
			t.Error("expected ok=false for removed job")
		}
	})

	t.Run("insertByPriorityLocked", func(t *testing.T) {
		q := New()
		jLow := &Job{ID: "low", Priority: constants.LowPriority}
		jHigh := &Job{ID: "high", Priority: constants.HighPriority}
		jNorm := &Job{ID: "norm", Priority: constants.NormalPriority}

		q.mu.Lock()
		defer q.mu.Unlock()

		q.insertByPriorityLocked(jLow)
		q.insertByPriorityLocked(jHigh)
		q.insertByPriorityLocked(jNorm)

		// Expected order descending: high, norm, low
		if len(q.jobs) != 3 || q.jobs[0].ID != "high" || q.jobs[1].ID != "norm" || q.jobs[2].ID != "low" {
			ids := make([]string, 0, len(q.jobs))
			for _, j := range q.jobs {
				ids = append(ids, j.ID)
			}
			t.Errorf("unexpected jobs order: %v", ids)
		}
	})

	t.Run("notifyLocked non-blocking", func(t *testing.T) {
		q := New()
		q.mu.Lock()
		defer q.mu.Unlock()

		q.notifyLocked()
		q.notifyLocked()
	})
}

func TestRemove_DeletesDiskFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create the jobs directory inside dir
	if err := os.MkdirAll(filepath.Join(dir, "jobs"), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	q := New()
	q.stateDir = dir // pass stateDir
	job := makeJob(t, "a", constants.NormalPriority)
	_ = q.Add(job)

	// Create a mock state file
	jobPath := filepath.Join(dir, "jobs", job.ID+".json.gz")
	if err := os.WriteFile(jobPath, []byte("mock data"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(jobPath); err != nil {
		t.Fatalf("mock file does not exist: %v", err)
	}

	// Remove the job
	if err := q.Remove(job.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Verify file is deleted
	if _, err := os.Stat(jobPath); !os.IsNotExist(err) {
		t.Fatalf("expected job file to be deleted, got err: %v", err)
	}
}

// TestRemove_NoIOUnderLock proves that Remove releases q.mu before calling
// removeFile. On the old code (lock held during delete), the read method
// called from the main goroutine blocks until Remove returns, causing the
// test to time out. After the fix the read returns immediately.
func TestRemove_NoIOUnderLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "jobs"), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	q := New()
	q.stateDir = dir
	job := makeJob(t, "lock-test", constants.NormalPriority)
	_ = q.Add(job)

	// Create a placeholder state file so removeFile has something to call.
	jobPath := filepath.Join(dir, "jobs", job.ID+".json.gz")
	if err := os.WriteFile(jobPath, []byte("placeholder"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Save the original removeFile hook.
	origRemoveFile := q.removeFile

	// started: closed when the hook begins executing (lock should be free by then).
	// release: closed by main goroutine to unblock the hook.
	started := make(chan struct{})
	release := make(chan struct{})

	q.removeFile = func(name string) error {
		close(started) // signal that we are inside the delete (lock must be released)
		<-release      // block until the main goroutine confirms the lock is free
		return origRemoveFile(name)
	}

	removeDone := make(chan struct{})
	go func() {
		defer close(removeDone)
		if err := q.Remove(job.ID); err != nil {
			t.Errorf("Remove: %v", err)
		}
	}()

	// Wait until the hook has started (i.e. Remove has at least reached the delete call).
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for removeFile hook to start")
	}

	// If the lock is still held here, q.Len() will block until Remove returns.
	// With the fix the lock is already released, so Len() returns promptly.
	lenDone := make(chan int, 1)
	go func() { lenDone <- q.Len() }()

	select {
	case n := <-lenDone:
		// Lock was not held — correct. Job was already removed from the in-memory
		// slice, so Len() should be 0.
		if n != 0 {
			t.Errorf("Len() = %d after Remove (pre-delete), want 0", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("q.Len() blocked — Remove is holding q.mu during disk I/O (bug not fixed)")
	}

	// Release the hook so Remove can finish.
	close(release)
	select {
	case <-removeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Remove goroutine did not finish after release")
	}
}

func TestMarkArticlesFailed_EmptyBatch(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeMultiFileJob(t, "fail-batch", 1, 3)
	_ = q.Add(j)

	t.Run("empty_batch_failed_is_no-op", func(t *testing.T) {
		q.dirty.Store(false)
		firstTime, err := q.MarkArticlesFailed(j.ID, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(firstTime) != 0 {
			t.Errorf("len(firstTime) = %d, want 0", len(firstTime))
		}
		if q.IsDirty() {
			t.Error("IsDirty should be false")
		}
	})

	t.Run("duplicate_failed_is_no-op", func(t *testing.T) {
		firstTime, err := q.MarkArticlesFailed(j.ID, []string{"f0a0@test"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(firstTime) != 1 || firstTime[0] != "f0a0@test" {
			t.Errorf("unexpected firstTime: %v", firstTime)
		}

		q.dirty.Store(false)
		firstTime2, err := q.MarkArticlesFailed(j.ID, []string{"f0a0@test"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(firstTime2) != 0 {
			t.Errorf("len(firstTime2) = %d, want 0", len(firstTime2))
		}
		if q.IsDirty() {
			t.Error("IsDirty should be false")
		}
	})
}

func TestMarkArticlesFailed_SignalsNotify(t *testing.T) {
	q := New()
	j := makeJob(t, "j", constants.NormalPriority)
	_ = q.Add(j)

	// Drain any signals triggered by Add.
	for {
		select {
		case <-q.Notify():
		default:
			goto drained
		}
	}
drained:

	msgID := j.Manifest().ArticleID(0)

	// 1. First-time failure: should signal notify.
	_, err := q.MarkArticlesFailed(j.ID, []string{msgID})
	if err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}

	select {
	case <-q.Notify():
		// Success
	case <-time.After(500 * time.Millisecond):
		t.Error("MarkArticlesFailed did not signal notify channel on first failure")
	}

	// 2. Repeat failure: should NOT signal notify.
	_, err = q.MarkArticlesFailed(j.ID, []string{msgID})
	if err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}

	select {
	case <-q.Notify():
		t.Error("MarkArticlesFailed signaled notify channel on repeat failure")
	default:
		// Success
	}
}

func TestQueue_PauseErrors(t *testing.T) {
	t.Parallel()

	q := New()

	// 1. Pause non-existent job
	err := q.Pause("non-existent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Pause non-existent: got %v, want %v", err, ErrNotFound)
	}

	// 2. Pause job in illegal state (e.g. Completed)
	j := makeJob(t, "j1", constants.NormalPriority)
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Must transition to Downloading -> Verifying -> Completed first to make it Completed
	if err := q.SetStatus(j.ID, constants.StatusDownloading); err != nil {
		t.Fatalf("SetStatus(Downloading): %v", err)
	}
	if err := q.SetStatus(j.ID, constants.StatusVerifying); err != nil {
		t.Fatalf("SetStatus(Verifying): %v", err)
	}
	if err := q.SetStatus(j.ID, constants.StatusCompleted); err != nil {
		t.Fatalf("SetStatus(Completed): %v", err)
	}

	err = q.Pause(j.ID)
	if !errors.Is(err, ErrIllegalStatusTransition) {
		t.Errorf("Pause Completed job: got %v, want %v", err, ErrIllegalStatusTransition)
	}
}

func TestSetFileWriteCursor(t *testing.T) {
	q := New()
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "movie.mkv", Bytes: 300, Articles: []nzb.Article{
			{ID: "a1@x", Bytes: 100, Number: 1},
			{ID: "a2@x", Bytes: 100, Number: 2},
		}},
	}}
	job, err := NewJob(parsed, AddOptions{Filename: "m.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Add(job); err != nil {
		t.Fatal(err)
	}
	if err := q.SetFileWriteCursor(job.ID, 0, 4096); err != nil {
		t.Fatalf("SetFileWriteCursor: %v", err)
	}
	snap := q.SnapshotJob(job.ID)
	if snap.Progress().FileWriteCursor(0) != 4096 {
		t.Errorf("WriteCursor = %d, want 4096", snap.Progress().FileWriteCursor(0))
	}
	if !q.IsDirty() {
		t.Error("queue should be marked dirty after SetFileWriteCursor")
	}
}

func TestSetFileWriteCursor_Errors(t *testing.T) {
	q := New()
	if err := q.SetFileWriteCursor("nope", 0, 1); err == nil {
		t.Error("expected error for unknown job")
	}
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "f", Bytes: 100, Articles: []nzb.Article{{ID: "a@x", Bytes: 100, Number: 1}}},
	}}
	job, _ := NewJob(parsed, AddOptions{Filename: "m.nzb"}, fsutil.SanitizeOptions{})
	_ = q.Add(job)
	if err := q.SetFileWriteCursor(job.ID, 5, 1); err == nil {
		t.Error("expected error for out-of-range fileIdx")
	}
}

func TestNew_WithLogger(t *testing.T) {
	l := slog.Default()
	q := New(WithLogger(l))
	if q.log == nil {
		t.Error("expected logger to be set via WithLogger, got nil")
	}
}

// TestSetPostProcStarted_DropsArtIndex pins the memory-hygiene invariant: once
// a job enters post-processing, its messageID->index map is released, since
// the download pipeline (the index's only consumer) is done with the job.
// articleIndexByID rebuilds it lazily if anything needs it afterward.
func TestSetPostProcStarted_DropsArtIndex(t *testing.T) {
	q := New()
	job := makeJob(t, "postproc-dropidx", constants.NormalPriority)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.SetStatus(job.ID, constants.StatusDownloading); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	id := job.Manifest().ArticleID(0)
	// Force the index to exist, as a real download would via MarkArticleDone.
	if _, ok := job.manifest.articleIndexByID(id); !ok {
		t.Fatal("articleIndexByID returned false for a present article")
	}
	if job.manifest.messageIDIndex == nil {
		t.Fatal("precondition: index should be built after articleIndexByID")
	}
	if _, err := q.SetPostProcStarted(job.ID); err != nil {
		t.Fatalf("SetPostProcStarted: %v", err)
	}
	if job.manifest.messageIDIndex != nil {
		t.Fatal("messageIDIndex should be dropped once the job enters post-processing")
	}
	// Round-trip: articleIndexByID must still work after the drop, rebuilding
	// the index lazily rather than leaving it permanently gone.
	idx, ok := job.manifest.articleIndexByID(id)
	if !ok || job.manifest.ArticleID(idx) != id {
		t.Fatalf("articleIndexByID(%q) after drop = (%v, %v), want a match", id, idx, ok)
	}
	if job.manifest.messageIDIndex == nil {
		t.Fatal("messageIDIndex should be rebuilt after articleIndexByID is called post-drop")
	}
}
