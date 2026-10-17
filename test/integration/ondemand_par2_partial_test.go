//go:build integration

package integration

import (
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
}

// writePar2Fixture writes a 600 KiB data file and a par2 set for it into dir:
// an index and recovery volumes of 1, 2, 4 and 8 blocks (16 KiB blocks, 15
// recovery blocks in all). It returns the payload.
func writePar2Fixture(t *testing.T, dir string) []byte {
	t.Helper()
	payload := make([]byte, 600*1024)
	rand.New(rand.NewSource(982)).Read(payload) //nolint:gosec // G404: deterministic test data
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), payload, 0o600); err != nil {
		t.Fatalf("write data.bin: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "par2", "create", "-q", "-s16384", "-c15", "-n4", "data.par2", "data.bin")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("par2 create: %v\n%s", err, out)
	}
	return payload
}

// runOnDemandPar2 serves the fixture in dir (after dropping the articles named
// by dropArticle, as "file:partNum") to a fresh app, runs one PP=3 job and
// reports what the server saw.
func runOnDemandPar2(t *testing.T, dir string, wantSHA [sha256.Size]byte, dropArticle ...string) onDemandRun {
	t.Helper()

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
	return run
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
	payload := writePar2Fixture(t, fixtureDir)
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
	for _, vol := range []string{"data.vol00+1.par2", "data.vol01+2.par2"} {
		if served(vol) == 0 {
			t.Errorf("%s was not fetched; the covering volumes for a 3-block deficit are +1 and +2", vol)
		}
	}
	for _, vol := range []string{"data.vol03+4.par2", "data.vol07+8.par2"} {
		if n := served(vol); n != 0 {
			t.Errorf("%s: %d articles fetched, want none (not part of the cheapest cover)", vol, n)
		}
	}
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
	t.Logf("extra round, gap from last data.bin BODY to first recovery-volume BODY: %v", gap)
	t.Logf("recovery volumes fetched over %v (first to last BODY request)", damaged.lastVolume.Sub(damaged.firstVolume))
	t.Logf("job wall clock AddJob to PostProcComplete: damaged %v, control %v (excess %v includes volume fetch and par2 repair)",
		damaged.total, control.total, damaged.total-control.total)
}
