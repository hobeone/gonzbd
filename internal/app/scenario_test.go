package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
)

// scenarioHarness wires a full app pipeline over a scripted NNTP fake for
// end-to-end state-machine tests. It owns tempdirs, a history Repository,
// an Application, and drains the completion channels into replayable
// logs so assertions do not race the consumer.
//
// Lifecycle: newScenarioHarness → Start → (AddSimpleJob / InjectFailure /
// WaitUntil) → Stop. Cleanup is registered on the testing.TB so callers do
// not need to call Stop explicitly unless they want to assert post-Stop.
type scenarioHarness struct {
	t       testing.TB
	server  *nntptest.Scripted
	app     *app.Application
	repo    *history.Repository
	closeDB func() error

	adminDir    string
	downloadDir string
	completeDir string

	ctx    context.Context
	cancel context.CancelFunc

	// Event logs: each completion channel is drained into a slice under mu.
	mu        sync.Mutex
	jobEvents []app.JobComplete
	ppEvents  []app.PostProcComplete

	drainWG sync.WaitGroup

	stopOnce sync.Once
}

func newScenarioHarness(t testing.TB) *scenarioHarness {
	return newScenarioHarnessWithConns(t, 2)
}

func newScenarioHarnessWithConns(t testing.TB, conns int) *scenarioHarness {
	t.Helper()

	h := &scenarioHarness{
		t:           t,
		server:      nntptest.New(t),
		adminDir:    t.TempDir(),
		downloadDir: t.TempDir(),
		completeDir: t.TempDir(),
	}

	db, err := history.Open(t.Context(), filepath.Join(h.adminDir, "history.db"))
	if err != nil {
		t.Fatalf("scenario: open history db: %v", err)
	}
	h.repo = history.NewRepository(db)
	h.closeDB = db.Close

	cfg := testConfig(
		h.downloadDir,
		h.completeDir,
		h.adminDir,
		h.server.ServerConfig("scenario", conns),
	)

	a, err := app.New(cfg, h.repo, app.WithPostProcStages([]postproc.Stage{noOpStage{}}))
	if err != nil {
		t.Fatalf("scenario: app.New: %v", err)
	}
	h.app = a

	t.Cleanup(func() { h.Stop() })
	return h
}

// Start brings up the application and begins draining the three completion
// channels into the harness event logs.
func (h *scenarioHarness) Start() {
	h.t.Helper()
	h.ctx, h.cancel = context.WithCancel(h.t.Context())
	if err := h.app.Start(h.ctx); err != nil {
		h.t.Fatalf("scenario: app.Start: %v", err)
	}

	h.drainWG.Add(2)
	go h.drainJobs()
	go h.drainPostProc()
}

// Stop shuts the application down and drains pending events. Safe to call
// multiple times.
func (h *scenarioHarness) Stop() {
	h.stopOnce.Do(func() {
		if h.app != nil {
			_ = h.app.Shutdown()
		}
		if h.cancel != nil {
			h.cancel()
		}
		h.drainWG.Wait()
		if h.closeDB != nil {
			_ = h.closeDB()
		}
	})
}

// AddSimpleJob builds a single-file, single-article job whose body is the
// provided raw bytes, posts the body to the scripted server, enqueues the
// job, and returns it. The file subject encodes the part framing yEnc
// expects.
func (h *scenarioHarness) AddSimpleJob(name string, raw []byte) *queue.Job {
	h.t.Helper()
	msgID := randomMsgID(h.t)

	body := yencSinglePart(name+".bin", raw)
	h.server.AddArticle(msgID, body)

	parsed := &nzb.NZB{
		Files: []nzb.File{{
			Subject:  fmt.Sprintf(`"%s.bin" yEnc (1/1)`, name),
			Articles: []nzb.Article{{ID: msgID, Bytes: len(raw), Number: 1}},
			Bytes:    int64(len(raw)),
		}},
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: name}, fsutil.SanitizeOptions{})
	if err != nil {
		h.t.Fatalf("scenario: NewJob: %v", err)
	}
	if err := h.app.Queue().Add(job); err != nil {
		h.t.Fatalf("scenario: Queue.Add: %v", err)
	}
	return job
}

// AddOneShotJob is like AddSimpleJob but uses Application.AddJob, which
// triggers duplicate detection and renaming logic.
func (h *scenarioHarness) AddOneShotJob(name string, raw []byte, force bool) *queue.Job {
	h.t.Helper()
	msgID := randomMsgID(h.t)

	body := yencSinglePart(name+".bin", raw)
	h.server.AddArticle(msgID, body)

	parsed := &nzb.NZB{
		Files: []nzb.File{{
			Subject:  fmt.Sprintf(`"%s.bin" yEnc (1/1)`, name),
			Articles: []nzb.Article{{ID: msgID, Bytes: len(raw), Number: 1}},
			Bytes:    int64(len(raw)),
		}},
	}
	// We must supply a Filename to trigger duplicate detection
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: name, Filename: name + ".nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		h.t.Fatalf("scenario: NewJob: %v", err)
	}

	// We pass rawNZB as dummy XML since we are simulating the ingestion of the NZB.
	if err := h.app.AddJob(h.t.Context(), job, []byte("<nzb/>"), force); err != nil {
		h.t.Fatalf("scenario: AddJob: %v", err)
	}
	return job
}

// InjectFailure wires a one-shot failure on the scripted server for msgID.
func (h *scenarioHarness) InjectFailure(msgID string, mode nntptest.FailureMode) {
	h.server.InjectFailure(msgID, mode)
}

// WaitForPostProc blocks until a PostProcComplete event is recorded for
// jobID or the timeout elapses. Returns true on success.
func (h *scenarioHarness) WaitForPostProc(jobID string, timeout time.Duration) bool {
	return h.waitFor(timeout, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, e := range h.ppEvents {
			if e.JobID == jobID {
				return true
			}
		}
		return false
	})
}

// WaitForHistory blocks until the history repository contains jobID.
func (h *scenarioHarness) WaitForHistory(jobID string, timeout time.Duration) bool {
	return h.waitFor(timeout, func() bool {
		_, err := h.repo.Get(h.t.Context(), jobID)
		return err == nil
	})
}

// WaitUntil polls cond at ~10ms intervals until it returns true or the
// timeout elapses. Returns true iff cond returned true.
func (h *scenarioHarness) WaitUntil(timeout time.Duration, cond func() bool) bool {
	return h.waitFor(timeout, cond)
}

func (h *scenarioHarness) waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// QueueContains reports whether jobID is currently in the active queue.
func (h *scenarioHarness) QueueContains(jobID string) bool {
	return h.app.Queue().SnapshotJob(jobID) != nil
}

// Events returns copies of the recorded completion event slices.
func (h *scenarioHarness) Events() (jobs []app.JobComplete, pps []app.PostProcComplete) {
	h.mu.Lock()
	defer h.mu.Unlock()
	jobs = append(jobs, h.jobEvents...)
	pps = append(pps, h.ppEvents...)
	return
}

func (h *scenarioHarness) drainJobs() {
	defer h.drainWG.Done()
	for e := range chanToReader(h.ctx, h.app.JobComplete()) {
		h.mu.Lock()
		h.jobEvents = append(h.jobEvents, e)
		h.mu.Unlock()
	}
}

func (h *scenarioHarness) drainPostProc() {
	defer h.drainWG.Done()
	for e := range chanToReader(h.ctx, h.app.PostProcComplete()) {
		h.mu.Lock()
		h.ppEvents = append(h.ppEvents, e)
		h.mu.Unlock()
	}
}

// chanToReader forwards values from src to a closed-on-ctx-done channel so
// drainers can use a simple `for range` loop. Does not close the source.
func chanToReader[T any](ctx context.Context, src <-chan T) <-chan T {
	out := make(chan T)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case v, ok := <-src:
				if !ok {
					return
				}
				select {
				case out <- v:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// noOpStage is a postproc.Stage that succeeds without performing any work.
// Used by scenario tests to move jobs through post-processing without
// invoking real par2/unrar on synthetic article bodies.
type noOpStage struct{}

func (noOpStage) Name() string                                 { return "noop" }
func (noOpStage) Run(_ context.Context, _ *postproc.Job) error { return nil }

// yencSinglePart encodes raw as a complete single-part yEnc article body,
// suitable for serving from the scripted NNTP fake.
func yencSinglePart(name string, raw []byte) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "=ybegin line=128 size=%d name=%s\r\n", len(raw), name)
	buf.Write(yencBody(raw))
	checksum := crc32.ChecksumIEEE(raw)
	fmt.Fprintf(&buf, "=yend size=%d crc32=%08x\r\n", len(raw), checksum)
	return buf.Bytes()
}

// yencMultiPart encodes raw as one part of a multi-part yEnc article body.
//
// The =ypart header is what makes it multi-part, and it is the reason the
// durability design needs a persisted article -> byte-range map at all: the
// NZB carries only an encoded byte count, while the decoded begin/end offsets
// live here, inside the article body, and are therefore downloaded data.
//
// begin and end are 1-based and inclusive, matching the yEnc spec.
func yencMultiPart(name string, raw []byte, part, total int, totalSize int64) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "=ybegin part=%d total=%d line=128 size=%d name=%s\r\n",
		part, total, totalSize, name)
	begin := int64(part-1)*int64(len(raw)) + 1
	fmt.Fprintf(&buf, "=ypart begin=%d end=%d\r\n", begin, begin+int64(len(raw))-1)
	buf.Write(yencBody(raw))
	fmt.Fprintf(&buf, "=yend size=%d part=%d pcrc32=%08x\r\n",
		len(raw), part, crc32.ChecksumIEEE(raw))
	return buf.Bytes()
}

// yencBody encodes raw into wrapped yEnc lines, shared by the single-part and
// multi-part helpers so the two cannot drift in their escaping.
func yencBody(raw []byte) []byte {
	encoded := make([]byte, 0, len(raw)+len(raw)/32)
	for _, b := range raw {
		enc := byte((int(b) + 42) % 256)
		if enc == 0 || enc == '\n' || enc == '\r' || enc == '=' {
			encoded = append(encoded, '=')
			enc = byte((int(enc) + 64) % 256)
		}
		encoded = append(encoded, enc)
	}
	var out bytes.Buffer
	const lineLen = 128
	for i := 0; i < len(encoded); i += lineLen {
		end := min(i+lineLen, len(encoded))
		out.Write(encoded[i:end])
		out.WriteString("\r\n")
	}
	return out.Bytes()
}

func randomMsgID(t testing.TB) string {
	t.Helper()
	var b [8]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		t.Fatalf("scenario: rand: %v", err)
	}
	return hex.EncodeToString(b[:]) + "@scenario"
}
