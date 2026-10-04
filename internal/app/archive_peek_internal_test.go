package app

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // par2 packet checksums are MD5 by specification
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/dispatch"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// peekFile is one file of a peek fixture job: the subject the NZB names it
// by, and the bytes the completed file holds.
type peekFile struct {
	subject string
	data    []byte
}

func unpackFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../unpack/testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// par2Packet builds one par2 packet: the 64-byte header (magic, length, MD5,
// recovery set ID, type) followed by body.
func par2Packet(typ [16]byte, body []byte) []byte {
	setID := [16]byte{7}
	sum := md5.New() //nolint:gosec // par2 packet checksum
	sum.Write(setID[:])
	sum.Write(typ[:])
	sum.Write(body)
	var out bytes.Buffer
	out.WriteString("PAR2\x00PKT")
	_ = binary.Write(&out, binary.LittleEndian, uint64(64+len(body)))
	out.Write(sum.Sum(nil))
	out.Write(setID[:])
	out.Write(typ[:])
	out.Write(body)
	return out.Bytes()
}

// par2Declaring builds a par2 file whose one File Description packet names
// name. Only the name matters to the peek; the rest is well-formed filler.
func par2Declaring(name string) []byte {
	typ := [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'F', 'i', 'l', 'e', 'D', 'e', 's', 'c'}
	var body bytes.Buffer
	body.Write(make([]byte, 16+16+16)) // file ID, MD5, MD5 of the first 16 KiB
	_ = binary.Write(&body, binary.LittleEndian, uint64(1024))
	body.WriteString(name)
	for body.Len()%4 != 0 {
		body.WriteByte(0)
	}
	return par2Packet(typ, body.Bytes())
}

type peekApp struct {
	*Application
	j    *job.Job
	repo *history.Repository
}

// newPeekApp builds a running application whose dispatcher launches nothing
// (holdingRunner), with one job at Fetching whose files are already written
// to its download directory, none yet complete. The unwanted rules are
// blacklist exts under action, and DirectUnpack is on when du is set. A
// fail-action job reaches history through peekNoOpStage.
func newPeekApp(t *testing.T, action unwanted.Action, exts []string, du bool, files []peekFile) *peekApp {
	t.Helper()
	application, repo, _ := newLifecycleTestApp(t, WithPostProcStages([]postproc.Stage{peekNoOpStage{}}))
	application.config.With(func(c *config.Config) {
		c.Downloads.UnwantedExtensions = exts
		c.Downloads.UnwantedExtensionsMode = unwanted.ModeBlacklist
		c.Downloads.ActionOnUnwantedExtensions = action
		c.PostProc.DirectUnpack = du
		c.PostProc.EnableUnrar = du
	})
	application.ctx = t.Context()
	runner := holdingRunner{launched: make(chan string, 16)}
	d := dispatch.New(
		1, 1, 10*time.Millisecond, time.Now,
		&appWorkers{app: application},
		application.residency,
		dispatchstore.New(repo.DB()),
		runner,
	)
	application.dispatcher = d
	application.pipeline.dispatcher = d
	t.Cleanup(application.duOrch.abortAll)

	parsed := &nzb.NZB{}
	for i, f := range files {
		parsed.Files = append(parsed.Files, nzb.File{
			Subject:  f.subject,
			Bytes:    int64(len(f.data)),
			Articles: []nzb.Article{{ID: fmt.Sprintf("p%d@t", i), Bytes: len(f.data), Number: 1}},
		})
	}
	j, hdr, err := BuildIngestJob(application.config, parsed, "peek.nzb", types.FetchOptions{NzbName: "peek"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	hdr.PP = 3
	if err := j.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if err := d.Add(t.Context(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	if err := application.postProcessor.Start(t.Context()); err != nil {
		t.Fatalf("postProcessor.Start: %v", err)
	}
	t.Cleanup(func() { _ = application.postProcessor.Stop() })
	select {
	case <-runner.launched:
	case <-time.After(10 * time.Second):
		t.Fatal("the job was never launched at Fetching")
	}

	dir := filepath.Join(application.config.GetGeneral().DownloadDir, j.Name())
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for i, f := range files {
		path := filepath.Join(dir, f.subject)
		if err := os.WriteFile(path, f.data, 0o600); err != nil {
			t.Fatal(err)
		}
		application.pipeline.mu.Lock()
		application.pipeline.fileInfo[fileKey{jobID: j.ID(), fileIdx: i}] = assembler.FileInfo{Path: path}
		application.pipeline.mu.Unlock()
	}
	return &peekApp{Application: application, j: j, repo: repo}
}

func (a *peekApp) complete(t *testing.T, i int) {
	t.Helper()
	if err := a.completeFinalizedFile(t.Context(), FileComplete{JobID: a.j.ID(), FileIdx: i}); err != nil {
		t.Fatalf("completeFinalizedFile(%d): %v", i, err)
	}
}

func (a *peekApp) state(t *testing.T) unwanted.State {
	t.Helper()
	st, ok := a.dispatcher.UnwantedState(a.j.ID())
	if !ok {
		t.Fatal("the job is not registered")
	}
	return st
}

// awaitHistory returns the job's history entry once the finalizer has filed
// it.
func (a *peekApp) awaitHistory(t *testing.T) history.Entry {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if e, err := a.repo.Get(t.Context(), a.j.ID()); err == nil && e.NzoID != "" {
			return *e
		}
		if time.Now().After(deadline) {
			t.Fatal("the job never reached history")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// peekNoOpStage succeeds without work: a job that reaches post-processing
// with a failure message skips every stage, so this only gives the
// post-processor a stage list.
type peekNoOpStage struct{}

func (peekNoOpStage) Name() string                                 { return "noop" }
func (peekNoOpStage) Run(_ context.Context, _ *postproc.Job) error { return nil }

// onlyTxt is the rule list the single_rar5.rar fixture trips: it holds
// file1.txt, file2.txt and nested.txt.
var onlyTxt = []string{"txt"}

func TestPeek_RARVolumeWithUnwantedMember_PausesTheJob(t *testing.T) {
	t.Parallel()
	a := newPeekApp(t, unwanted.ActionPause, onlyTxt, false,
		[]peekFile{{"release.part1.rar", unpackFixture(t, "single_rar5.rar")}})
	a.complete(t, 0)

	if got := a.state(t); got != unwanted.StateBlocked {
		t.Fatalf("Unwanted = %d, want blocked", got)
	}
	if in := a.j.Intent(); in != job.IntentPause {
		t.Errorf("Intent = %v, want IntentPause", in)
	}
	if _, err := a.j.Manifest(); err != nil || !a.j.IsComplete() {
		t.Errorf("the flagged file was not marked complete (complete=%v, manifest err=%v)", a.j.IsComplete(), err)
	}
	// Blocked reaches dispatch_jobs by the next tick, as the ingest check's does.
	store := dispatchstore.New(a.repo.DB())
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, err := store.Load(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].Header.Unwanted == unwanted.StateBlocked && rows[0].Intent == job.IntentPause {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dispatch_jobs never recorded blocked+paused: %+v", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The user's resume approves it, as for the ingest check.
	if err := a.dispatcher.ResumeJobByUser(a.j.ID()); err != nil {
		t.Fatalf("ResumeJobByUser: %v", err)
	}
	if got := a.state(t); got != unwanted.StateApproved {
		t.Errorf("after the user's resume Unwanted = %d, want approved", got)
	}
}

func TestPeek_RARVolumeWithUnwantedMember_FailsTheJob(t *testing.T) {
	t.Parallel()
	a := newPeekApp(t, unwanted.ActionFail, onlyTxt, false,
		[]peekFile{{"release.part1.rar", unpackFixture(t, "single_rar5.rar")}})
	a.complete(t, 0)

	e := a.awaitHistory(t)
	if e.Status != string(constants.StatusFailed) {
		t.Errorf("Status = %q, want Failed", e.Status)
	}
	want := "Aborted, unwanted extension detected: file1.txt, file2.txt, nested.txt"
	if e.FailMessage != want {
		t.Errorf("FailMessage = %q, want %q", e.FailMessage, want)
	}
	if e.Unwanted != unwanted.StateBlocked {
		t.Errorf("history Unwanted = %d, want blocked", e.Unwanted)
	}
	if _, err := os.Stat(filepath.Join(a.config.GetGeneral().DownloadDir, a.j.Name(), "release.part1.rar")); err != nil {
		t.Errorf("the downloaded file was not kept: %v", err)
	}
}

func TestPeek_ObfuscatedNameIsIdentifiedByMagic(t *testing.T) {
	t.Parallel()
	a := newPeekApp(t, unwanted.ActionPause, onlyTxt, false,
		[]peekFile{{"a1b2c3d4e5f6", unpackFixture(t, "single_rar5.rar")}})
	a.complete(t, 0)
	if got := a.state(t); got != unwanted.StateBlocked {
		t.Fatalf("Unwanted = %d, want blocked: a RAR with an obfuscated name was not read", got)
	}
}

func TestPeek_Par2DeclaredNameBlocks(t *testing.T) {
	t.Parallel()
	a := newPeekApp(t, unwanted.ActionPause, []string{"exe"}, false,
		[]peekFile{{"release.par2", par2Declaring("payload.exe")}})
	a.complete(t, 0)
	if got := a.state(t); got != unwanted.StateBlocked {
		t.Fatalf("Unwanted = %d, want blocked by the par2 file's declared name", got)
	}
}

func TestPeek_Par2WithWantedNameDoesNotBlock(t *testing.T) {
	t.Parallel()
	a := newPeekApp(t, unwanted.ActionPause, []string{"exe"}, false,
		[]peekFile{{"release.par2", par2Declaring("movie.mkv")}})
	a.complete(t, 0)
	if got := a.state(t); got != unwanted.StateNone {
		t.Fatalf("Unwanted = %d, want none", got)
	}
}

func TestPeek_SkipsWhenItShould(t *testing.T) {
	t.Parallel()
	rar := unpackFixture(t, "single_rar5.rar")
	cases := []struct {
		name   string
		action unwanted.Action
		files  []peekFile
		setup  func(t *testing.T, a *peekApp)
	}{
		{name: "action off", action: unwanted.ActionOff, files: []peekFile{{"x.rar", rar}}},
		{name: "a failed article in the file", action: unwanted.ActionPause, files: []peekFile{{"x.rar", rar}},
			setup: func(t *testing.T, a *peekApp) {
				if err := a.j.MarkArticleFailed(0); err != nil {
					t.Fatalf("MarkArticleFailed: %v", err)
				}
			}},
		{name: "a corrupt volume", action: unwanted.ActionPause, files: []peekFile{{"x.rar", unpackFixture(t, "corrupt.rar")}}},
		{name: "not an archive", action: unwanted.ActionPause, files: []peekFile{{"x.rar", []byte("just some bytes")}}},
		{name: "header-encrypted rar", action: unwanted.ActionPause, files: []peekFile{{"x.rar", unpackFixture(t, "encrypted_header.rar")}}},
		{name: "empty file", action: unwanted.ActionPause, files: []peekFile{{"x.rar", nil}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newPeekApp(t, tc.action, onlyTxt, false, tc.files)
			if tc.setup != nil {
				tc.setup(t, a)
			}
			a.complete(t, 0)
			if got := a.state(t); got != unwanted.StateNone {
				t.Errorf("Unwanted = %d, want none", got)
			}
			if in := a.j.Intent(); in != job.IntentRun {
				t.Errorf("Intent = %v, want IntentRun", in)
			}
			if !a.j.IsComplete() {
				t.Error("the file was not marked complete: the skip must not affect the job")
			}
		})
	}
}

func TestPeek_ApprovedJobIsNotChecked(t *testing.T) {
	t.Parallel()
	a := newPeekApp(t, unwanted.ActionPause, onlyTxt, false,
		[]peekFile{{"x.rar", unpackFixture(t, "single_rar5.rar")}})
	// The user approves before the volume completes.
	if _, err := a.dispatcher.BlockUnwanted(a.j.ID(), false); err != nil {
		t.Fatal(err)
	}
	if err := a.dispatcher.ResumeJobByUser(a.j.ID()); err != nil {
		t.Fatal(err)
	}
	a.complete(t, 0)
	if got := a.state(t); got != unwanted.StateApproved {
		t.Fatalf("Unwanted = %d, want approved", got)
	}
	if in := a.j.Intent(); in != job.IntentRun {
		t.Errorf("Intent = %v, want IntentRun", in)
	}
}

// TestPeek_ConcurrentCompletionsActOnce pins that two volumes completing at
// once, both carrying hits, produce one block and one action.
func TestPeek_ConcurrentCompletionsActOnce(t *testing.T) {
	t.Parallel()
	rar := unpackFixture(t, "single_rar5.rar")
	a := newPeekApp(t, unwanted.ActionFail, onlyTxt, false,
		[]peekFile{{"a.rar", rar}, {"b.rar", rar}, {"c.rar", rar}, {"d.rar", rar}})
	var acted atomic.Int32
	a.log = slog.New(countingHandler{needle: "downloaded archive names files", n: &acted})

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 4 {
		wg.Go(func() {
			<-start
			// The peek itself, not the whole completion: a completion that
			// lands after the failed job was handed to post-processing finds
			// it no longer resident, which is not what is under test.
			a.peekArchiveForUnwanted(a.j, FileComplete{JobID: a.j.ID(), FileIdx: i})
		})
	}
	close(start)
	wg.Wait()

	if got := acted.Load(); got != 1 {
		t.Errorf("the block acted %d times, want 1", got)
	}
	e := a.awaitHistory(t)
	if e.Status != string(constants.StatusFailed) || e.Unwanted != unwanted.StateBlocked {
		t.Errorf("history = status %q unwanted %d, want Failed and blocked", e.Status, e.Unwanted)
	}
}

// TestBlockForUnwanted_ALoserOfTheRaceDoesNotAct is the race above made
// deterministic: a completion that read StateNone before another moved the
// job reaches the acting half after the move, and must not act again.
func TestBlockForUnwanted_ALoserOfTheRaceDoesNotAct(t *testing.T) {
	t.Parallel()
	a := newPeekApp(t, unwanted.ActionFail, onlyTxt, false,
		[]peekFile{{"a.rar", unpackFixture(t, "single_rar5.rar")}})
	var acted atomic.Int32
	a.log = slog.New(countingHandler{needle: "downloaded archive names files", n: &acted})

	for range 2 {
		a.blockForUnwanted(a.j.ID(), 0, "rar", unwanted.ActionFail, []string{"file1.txt"})
	}
	if got := acted.Load(); got != 1 {
		t.Errorf("the block acted %d times for two calls on one job, want 1", got)
	}
	e := a.awaitHistory(t)
	if e.FailMessage != "Aborted, unwanted extension detected: file1.txt" {
		t.Errorf("FailMessage = %q", e.FailMessage)
	}
}

// TestBlockForUnwanted_Failures pins the two ways the dispatcher's answer is
// an error: a job that is not registered is neither blocked nor acted on, and
// a job whose pause is refused (it is cancelled) is still recorded blocked.
func TestBlockForUnwanted_Failures(t *testing.T) {
	t.Parallel()
	a := newPeekApp(t, unwanted.ActionPause, onlyTxt, false,
		[]peekFile{{"a.rar", unpackFixture(t, "single_rar5.rar")}})
	var acted atomic.Int32
	a.log = slog.New(countingHandler{needle: "downloaded archive names files", n: &acted})

	a.blockForUnwanted("no-such-job", 0, "rar", unwanted.ActionPause, []string{"x.txt"})
	if got := acted.Load(); got != 0 {
		t.Errorf("an unregistered job was acted on %d times", got)
	}

	if err := a.j.SetIntent(job.IntentCancel); err != nil {
		t.Fatal(err)
	}
	a.blockForUnwanted(a.j.ID(), 0, "rar", unwanted.ActionPause, []string{"x.txt"})
	if got := a.state(t); got != unwanted.StateBlocked {
		t.Errorf("Unwanted = %d, want blocked even though the pause was refused", got)
	}
}

// countingHandler counts the records whose message contains needle.
type countingHandler struct {
	needle string
	n      *atomic.Int32
}

func (countingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h countingHandler) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, h.needle) {
		h.n.Add(1)
	}
	return nil
}
func (h countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h countingHandler) WithGroup(string) slog.Handler      { return h }

// TestPeek_FlaggedVolumeIsNotFedToDirectUnpack pins that the volume the peek
// flags never reaches an unpacker: no unpacker exists for the job afterwards.
func TestPeek_FlaggedVolumeIsNotFedToDirectUnpack(t *testing.T) {
	t.Parallel()
	files := make([]peekFile, 0, rarVolumes)
	for i := range rarVolumes {
		name := fmt.Sprintf("multi_new.part%02d.rar", i+1)
		files = append(files, peekFile{name, unpackFixture(t, name)})
	}
	// bigfile.bin is the member volume 1 lists.
	a := newPeekApp(t, unwanted.ActionPause, []string{"bin"}, true, files)
	a.complete(t, 0)
	if got := a.state(t); got != unwanted.StateBlocked {
		t.Fatalf("Unwanted = %d, want blocked", got)
	}
	if _, ok := a.duOrch.status(a.j.ID()); ok {
		t.Error("an unpacker exists for a job whose first volume was flagged")
	}
	// Later volumes of the blocked job are not fed either.
	a.complete(t, 1)
	if _, ok := a.duOrch.status(a.j.ID()); ok {
		t.Error("a later volume of a blocked job started an unpacker")
	}
}

// TestPeek_HitWhileDirectUnpackRuns_AbortsIt pins the pause path's decision:
// a hit that arrives after an unpacker has started (here a par2 file naming
// the unwanted member, completing after volume 1) aborts and removes it, so
// no flagged member is extracted from volumes that complete later.
func TestPeek_HitWhileDirectUnpackRuns_AbortsIt(t *testing.T) {
	t.Parallel()
	files := []peekFile{
		{"multi_new.part01.rar", unpackFixture(t, "multi_new.part01.rar")},
		{"release.par2", par2Declaring("payload.exe")},
	}
	a := newPeekApp(t, unwanted.ActionPause, []string{"exe"}, true, files)
	a.complete(t, 0)
	if _, ok := a.duOrch.status(a.j.ID()); !ok {
		t.Fatal("precondition: volume 1 did not start an unpacker")
	}
	a.complete(t, 1)
	if got := a.state(t); got != unwanted.StateBlocked {
		t.Fatalf("Unwanted = %d, want blocked", got)
	}
	if _, ok := a.duOrch.status(a.j.ID()); ok {
		t.Error("the job's unpacker survived the block")
	}
}

func TestPeek_WithoutADispatcherDoesNothing(t *testing.T) {
	t.Parallel()
	(&Application{}).peekArchiveForUnwanted(nil, FileComplete{})
}

func TestArchiveMemberNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	opts := par2.DefaultParseOptions()
	cases := []struct {
		name     string
		path     string
		wantKind string
		want     []string
		wantErr  bool
	}{
		{"rar5 members by magic, not by name", write("noext", unpackFixture(t, "single_rar5.rar")), "rar",
			[]string{"file1.txt", "file2.txt", "nested.txt"}, false},
		{"par2 declared names", write("x.bin", par2Declaring("a.exe")), "par2", []string{"a.exe"}, false},
		{"neither archive kind", write("plain", []byte("hello world, not an archive")), "", nil, false},
		{"corrupt rar is an error", write("bad.rar", unpackFixture(t, "corrupt.rar")), "rar", nil, true},
		{"missing file is an error", filepath.Join(dir, "missing"), "", nil, true},
	}
	for _, tc := range cases {
		got, kind, err := archiveMemberNames(tc.path, opts)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, want error: %v", tc.name, err, tc.wantErr)
		}
		if kind != tc.wantKind {
			t.Errorf("%s: kind = %q, want %q", tc.name, kind, tc.wantKind)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: names = %v, want %v", tc.name, got, tc.want)
		}
	}
}
