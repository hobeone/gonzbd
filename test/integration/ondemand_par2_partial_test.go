//go:build integration

package integration

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/test/mocknntp"
)

// onDemandRun is what one on-demand par2 job observed on the mock server.
type onDemandRun struct {
	// total is AddJob to PostProcComplete.
	total time.Duration
	// served is, per file name, how many of its articles were delivered in full.
	served map[string]int
	// lastContent is the time of the last BODY request for the data file,
	// firstVolume and lastVolume bracket the BODY requests for recovery
	// volumes. Zero when no volume was requested.
	lastContent, firstVolume, lastVolume time.Time
	stageLog                             string
	// retried is whether the finalizer retried the job after a failed repair
	// to fetch its held volumes, read from the log the run wrote.
	retried bool
}

// lockedBuffer is a log sink the app's goroutines and the test can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// writePar2Fixture writes a 600 KiB data file and a par2 set for it into dir:
// an index and recovery volumes of 1, 2, 4 and 8 blocks (16 KiB blocks, 15
// recovery blocks in all). It returns the payload and the volumes' names by
// block count.
//
// The sizes are par2cmdline's default scheme for 15 blocks, so no -n or -u is
// passed. A par2 whose default differs fails the fixture guard rather than an
// assertion about the cover, and the names are read back from the directory
// rather than spelled here, since the padding of "+MM" is the tool's choice.
func writePar2Fixture(t *testing.T, dir string) (payload []byte, volByBlocks map[int]string) {
	t.Helper()
	payload = make([]byte, 600*1024)
	rand.New(rand.NewSource(982)).Read(payload) //nolint:gosec // G404: deterministic test data
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), payload, 0o600); err != nil {
		t.Fatalf("write data.bin: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "par2", "create", "-q", "-s16384", "-c15", "data.par2", "data.bin")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("par2 create: %v\n%s", err, out)
	}
	vols, err := filepath.Glob(filepath.Join(dir, "data.vol*.par2"))
	if err != nil {
		t.Fatal(err)
	}
	volByBlocks = make(map[int]string, len(vols))
	for _, v := range vols {
		name := filepath.Base(v)
		if n, ok := job.RecoveryVolumeBlocks(name); ok {
			volByBlocks[n] = name
		}
	}
	for _, n := range []int{1, 2, 4, 8} {
		if _, ok := volByBlocks[n]; !ok || len(vols) != 4 {
			t.Fatalf("fixture guard: par2 create made %v, want one volume each of 1, 2, 4 and 8 blocks", vols)
		}
	}
	return payload, volByBlocks
}

// runOnDemandPar2 serves the fixture in dir (after dropping the articles named
// by dropArticle, as "file:partNum") to a fresh app, runs one PP=3 job and
// reports what the server saw.
func runOnDemandPar2(t *testing.T, dir string, wantSHA [sha256.Size]byte, dropArticle ...string) onDemandRun {
	t.Helper()

	// The app logs through the default logger, which writes via package log.
	// Not parallel-safe, like the timings: every test here runs serially.
	logs := &lockedBuffer{}
	log.SetOutput(io.MultiWriter(os.Stderr, logs))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	const partSize = 48 * 1024 // three 16 KiB par2 blocks per article
	files := fixtureToTestFiles(t, dir, partSize)

	bodyCalled := make(chan string, 1024)
	srv := mocknntp.NewServer(mocknntp.Config{OnBodyCalled: bodyCalled})
	owner := map[string]string{} // message-ID -> file name
	for _, f := range files {
		parts := SplitIntoParts(f.Payload, f.PartSize)
		var offset int64
		for i, part := range parts {
			n := i + 1
			mid, body := BuildArticle(part, f.Name, n, len(parts), int64(len(f.Payload)), offset)
			offset += int64(len(part))
			owner[mid] = f.Name
			if slices.Contains(dropArticle, fmt.Sprintf("%s:%d", f.Name, n)) {
				continue
			}
			srv.AddArticle(mid, body)
		}
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("mock server start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	var (
		mu    sync.Mutex
		run   onDemandRun
		isVol = func(name string) bool { return strings.Contains(name, ".vol") }
	)
	go func() {
		for {
			select {
			case mid := <-bodyCalled:
				now := time.Now()
				name := owner[mid]
				mu.Lock()
				switch {
				case isVol(name):
					if run.firstVolume.IsZero() {
						run.firstVolume = now
					}
					run.lastVolume = now
				case name == "data.bin":
					run.lastContent = now
				}
				mu.Unlock()
			case <-t.Context().Done():
				return
			}
		}
	}()

	a, _, completeDir := NewTestAppSeparateDirs(t, srv.Addr(), AppTestOpts{})
	rawNZB := BuildNZB(files)
	start := time.Now()
	j := addNZBJobPP(t, a, rawNZB, "partial-par2", 3)
	waitForPostProcWithTimeout(t, a, pipelineTimeout)
	total := time.Since(start)

	verifyFileAtPath(t, filepath.Join(completeDir, "partial-par2", "data.bin"), wantSHA[:])

	// The history row is filed by the finalizer, which can trail the
	// post-processing signal.
	var stageLog string
	for range 100 {
		if e, err := a.GetHistory(t.Context(), j.ID()); err == nil && e != nil && e.StageLog != "" {
			if e.Status != "Completed" {
				t.Fatalf("history status = %q (fail message %q), want Completed", e.Status, e.FailMessage)
			}
			stageLog = e.StageLog
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if stageLog == "" {
		t.Fatalf("no history entry with a stage log for job %s", j.ID())
	}

	delivered := srv.ArticlesServed()
	served := map[string]int{}
	for mid, n := range delivered {
		if n > 0 {
			served[owner[mid]]++
		}
	}
	mu.Lock()
	defer mu.Unlock()
	run.total = total
	run.served = served
	run.stageLog = stageLog
	run.retried = strings.Contains(logs.String(), "retrying the job to fetch them")
	return run
}

// requireReleasedAtAssessing fails t if the job reached its volumes through the
// finalizer's retry of a failed repair rather than through the Assessing
// verdict: a verdict that undercounted would still end with the right volumes,
// one failed repair later.
func requireReleasedAtAssessing(t *testing.T, run onDemandRun) {
	t.Helper()
	if run.retried {
		t.Errorf("the job was retried after a failed repair: the Assessing verdict released too few volumes:\n%s", run.stageLog)
	}
}

// TestOnDemandPar2_PartialRelease_FetchesOnlyTheCoveringVolumes posts a data
// file with a par2 set of 1-, 2-, 4- and 8-block recovery volumes and removes
// one interior article (three 16 KiB par2 blocks) from the server. The
// Assessing verdict must release the byte-minimal cover of that three-block
// deficit, the 1- and 2-block volumes (53 KB) rather than the 4-block one
// (69 KB), and the job must repair and finish.
//
// It logs two measurements of what the extra Fetching and Assessing round
// costs, both taken from the mock server's BODY requests and the job's wall
// clock, against a control job with every article present:
//
//   - gap: from the last BODY request for the data file to the first BODY
//     request for a recovery volume. That spans the download-complete
//     report, the Assessing verdict (par2.Assess over the directory and the
//     written-rows read) and the re-dispatch of the released volumes.
//   - total: AddJob to PostProcComplete for each job. The damaged job's
//     excess over the control also includes the volume downloads and the par2
//     repair itself, so it is an upper bound on the round, not the round.
//
// It is not parallel, so Go runs it before the parallel tests of the package
// and the timings are not taken under their load.
func TestOnDemandPar2_PartialRelease_FetchesOnlyTheCoveringVolumes(t *testing.T) {
	requireTool(t, "par2")

	fixtureDir := t.TempDir()
	payload, vol := writePar2Fixture(t, fixtureDir)
	wantSHA := sha256.Sum256(payload)

	// Article 5 of data.bin covers bytes [196608, 245760): blocks 12-14.
	damaged := runOnDemandPar2(t, fixtureDir, wantSHA, "data.bin:5")

	if got, want := damaged.served["data.bin"], 12; got != want {
		t.Errorf("data.bin articles served = %d, want %d (13 minus the dropped one)", got, want)
	}
	served := func(name string) int { return damaged.served[name] }
	if served("data.par2") == 0 {
		t.Errorf("the par2 index was not fetched")
	}
	for _, blocks := range []int{1, 2} {
		if served(vol[blocks]) == 0 {
			t.Errorf("%s was not fetched; the covering volumes for a 3-block deficit are +1 and +2", vol[blocks])
		}
	}
	for _, blocks := range []int{4, 8} {
		if n := served(vol[blocks]); n != 0 {
			t.Errorf("%s: %d articles fetched, want none (not part of the cheapest cover)", vol[blocks], n)
		}
	}
	requireReleasedAtAssessing(t, damaged)
	if !strings.Contains(damaged.stageLog, "fetched 2 of 4 recovery volume(s)") {
		t.Errorf("stage log does not report a partial release (want %q):\n%s", "fetched 2 of 4 recovery volume(s)", damaged.stageLog)
	}

	control := runOnDemandPar2(t, fixtureDir, wantSHA)
	if got, want := control.served["data.bin"], 13; got != want {
		t.Errorf("control: data.bin articles served = %d, want %d", got, want)
	}
	for name, n := range control.served {
		if strings.Contains(name, ".vol") {
			t.Errorf("control: %s: %d articles fetched, want none for a clean download", name, n)
		}
	}

	gap := damaged.firstVolume.Sub(damaged.lastContent)
	logGap(t, damaged, control, gap)
}

// TestOnDemandPar2_CRCMismatch_FetchesOnlyTheCoveringVolumes posts a data file
// that arrived with every article but one byte wrong: the articles carry the
// corrupt bytes with valid yEnc CRCs, so no article fails and the job has no
// gap, only a whole-file CRC that differs from par2's. A block-level verify
// finds one damaged block, so only the 1-block volume is released; the repair
// restores the original payload.
func TestOnDemandPar2_CRCMismatch_FetchesOnlyTheCoveringVolumes(t *testing.T) {
	requireTool(t, "par2")

	fixtureDir := t.TempDir()
	payload, vol := writePar2Fixture(t, fixtureDir)
	wantSHA := sha256.Sum256(payload)

	corrupt := slices.Clone(payload)
	corrupt[5*16384+10] ^= 0xFF // inside block 5
	if err := os.WriteFile(filepath.Join(fixtureDir, "data.bin"), corrupt, 0o600); err != nil {
		t.Fatalf("write the corrupt data.bin: %v", err)
	}

	run := runOnDemandPar2(t, fixtureDir, wantSHA)

	if got, want := run.served["data.bin"], 13; got != want {
		t.Errorf("data.bin articles served = %d, want %d (every article is delivered)", got, want)
	}
	if run.served[vol[1]] == 0 {
		t.Errorf("%s was not fetched; the covering volume for one damaged block is +1", vol[1])
	}
	for _, blocks := range []int{2, 4, 8} {
		if n := run.served[vol[blocks]]; n != 0 {
			t.Errorf("%s: %d articles fetched, want none (not part of the cheapest cover)", vol[blocks], n)
		}
	}
	requireReleasedAtAssessing(t, run)
	if !strings.Contains(run.stageLog, "fetched 1 of 4 recovery volume(s)") {
		t.Errorf("stage log does not report a partial release (want %q):\n%s", "fetched 1 of 4 recovery volume(s)", run.stageLog)
	}
}

// TestOnDemandPar2_LostTailArticle_FetchesOnlyTheCoveringVolumes drops the last
// article of data.bin (24 KiB: the last block and a half). The assembler
// preallocates the file, so the loss is a hole at its tail and the entry is
// sized by the written rows, not by a verify: two blocks, which the 2-block
// volume alone covers. The truncated file a verify sizes is covered by
// TestRecoveryDeficit.
func TestOnDemandPar2_LostTailArticle_FetchesOnlyTheCoveringVolumes(t *testing.T) {
	requireTool(t, "par2")

	fixtureDir := t.TempDir()
	payload, vol := writePar2Fixture(t, fixtureDir)
	wantSHA := sha256.Sum256(payload)

	run := runOnDemandPar2(t, fixtureDir, wantSHA, "data.bin:13")

	if got, want := run.served["data.bin"], 12; got != want {
		t.Errorf("data.bin articles served = %d, want %d (13 minus the dropped one)", got, want)
	}
	if run.served[vol[2]] == 0 {
		t.Errorf("%s was not fetched; the covering volume for two lost blocks is +2", vol[2])
	}
	for _, blocks := range []int{1, 4, 8} {
		if n := run.served[vol[blocks]]; n != 0 {
			t.Errorf("%s: %d articles fetched, want none (not part of the cheapest cover)", vol[blocks], n)
		}
	}
	requireReleasedAtAssessing(t, run)
}

// TestOnDemandPar2_NoIndex_FetchesOneVolumeAsTheIndex posts the same set
// without its index file. The Assessing verdict finds no par2 file on disk
// and fetches the single cheapest volume (+1) to serve as the index; the next
// Assessing sizes the rest from it. With one article (3 blocks) missing the
// deficit is 3, the +1 volume covers 1, so the +2 volume is released as well
// and +4 and +8 stay unfetched. With nothing missing, +1 alone is fetched.
func TestOnDemandPar2_NoIndex_FetchesOneVolumeAsTheIndex(t *testing.T) {
	requireTool(t, "par2")

	// An index posted but describing nothing is no index either: the volume
	// fetched as the index is read in its place, and go_par2 repairs from it
	// and its siblings without the external par2 (#1006).
	for _, tc := range []struct {
		name  string
		index []byte // nil: no data.par2 in the post
	}{
		{"no index", nil},
		{"a zero-filled index", make([]byte, 4096)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixtureDir := t.TempDir()
			payload, vol := writePar2Fixture(t, fixtureDir)
			wantSHA := sha256.Sum256(payload)
			indexPath := filepath.Join(fixtureDir, "data.par2")
			if tc.index == nil {
				if err := os.Remove(indexPath); err != nil {
					t.Fatalf("remove index: %v", err)
				}
			} else if err := os.WriteFile(indexPath, tc.index, 0o600); err != nil {
				t.Fatalf("overwrite index: %v", err)
			}

			check := func(t *testing.T, run onDemandRun, fetched, held []int) {
				t.Helper()
				if n := run.served["data.par2"]; tc.index == nil && n != 0 {
					t.Errorf("data.par2: %d articles fetched, want none (not in the NZB)", n)
				}
				for _, blocks := range fetched {
					if run.served[vol[blocks]] == 0 {
						t.Errorf("%s was not fetched", vol[blocks])
					}
				}
				for _, blocks := range held {
					if n := run.served[vol[blocks]]; n != 0 {
						t.Errorf("%s: %d articles fetched, want none", vol[blocks], n)
					}
				}
				if strings.Contains(run.stageLog, "retrying with external") {
					t.Errorf("the repair fell back to the external par2:\n%s", run.stageLog)
				}
			}

			t.Run("damaged", func(t *testing.T) {
				run := runOnDemandPar2(t, fixtureDir, wantSHA, "data.bin:5")
				if got, want := run.served["data.bin"], 12; got != want {
					t.Errorf("data.bin articles served = %d, want %d", got, want)
				}
				check(t, run, []int{1, 2}, []int{4, 8})
			})
			t.Run("clean", func(t *testing.T) {
				run := runOnDemandPar2(t, fixtureDir, wantSHA)
				check(t, run, []int{1}, []int{2, 4, 8})
			})
		})
	}
}

func logGap(t *testing.T, damaged, control onDemandRun, gap time.Duration) {
	t.Helper()
	t.Logf("extra round, gap from last data.bin BODY to first recovery-volume BODY: %v", gap)
	t.Logf("recovery volumes fetched over %v (first to last BODY request)", damaged.lastVolume.Sub(damaged.firstVolume))
	t.Logf("job wall clock AddJob to PostProcComplete: damaged %v, control %v (excess %v includes volume fetch and par2 repair)",
		damaged.total, control.total, damaged.total-control.total)
}
