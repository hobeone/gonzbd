package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
	"github.com/hobeone/gonzbd/internal/unwanted"

	"github.com/hobeone/gonzbd/internal/app"
)

// peekNZB renders a one-file, one-article NZB for a file called name whose
// article carries data, and serves that article from the harness's server.
func (h *scenarioHarness) peekNZB(name string, data []byte) []byte {
	h.t.Helper()
	msgID := randomMsgID(h.t)
	h.server.AddArticle(msgID, yencSinglePart(name, data))
	return fmt.Appendf(nil, `<?xml version="1.0" encoding="iso-8859-1" ?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
<file poster="p@t" date="1700000000" subject="&quot;%s&quot; yEnc (1/1)">
<groups><group>alt.bin.test</group></groups>
<segments><segment bytes="%d" number="1">%s</segment></segments>
</file>
</nzb>
`, name, len(data), msgID)
}

func (h *scenarioHarness) addRaw(filename string, raw []byte) *job.Job {
	h.t.Helper()
	parsed, err := nzb.Parse(bytes.NewReader(raw))
	if err != nil {
		h.t.Fatalf("nzb.Parse: %v", err)
	}
	j, hdr, err := app.BuildIngestJob(h.cfg, parsed, filename, types.FetchOptions{}, nil)
	if err != nil {
		h.t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := h.app.AddJob(h.t.Context(), j, hdr, raw, false); err != nil {
		h.t.Fatalf("AddJob: %v", err)
	}
	return j
}

func archivePeekHarness(t *testing.T, action unwanted.Action) *scenarioHarness {
	t.Helper()
	h := newScenarioHarnessWithConfig(t, 1, func(c *config.Config) {
		c.Downloads.UnwantedExtensions = []string{"txt"}
		c.Downloads.ActionOnUnwantedExtensions = action
	})
	h.Start()
	return h
}

func singleRAR(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "unpack", "testdata", "single_rar5.rar"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *scenarioHarness) historyEntry(jobID string) *history.Entry {
	h.t.Helper()
	if !h.WaitForHistory(jobID, 30*time.Second) {
		h.t.Fatalf("job %s never reached history", jobID)
	}
	e, err := h.repo.Get(h.t.Context(), jobID)
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

// TestArchivePeek_Pipeline_FailActionFiltersMidDownload drives the real
// pipeline: the downloaded RAR names a .txt member the NZB's subject does
// not, the job is filed Failed with the abort message and Blocked, and a
// retry with the user's approval then completes.
func TestArchivePeek_Pipeline_FailActionFiltersMidDownload(t *testing.T) {
	t.Parallel()
	h := archivePeekHarness(t, unwanted.ActionFail)
	raw := h.peekNZB("release.rar", singleRAR(t))
	j := h.addRaw("peek.nzb", raw)

	e := h.historyEntry(j.ID())
	if e.Status != string(constants.StatusFailed) {
		t.Fatalf("Status = %q (%q), want Failed", e.Status, e.FailMessage)
	}
	want := "Aborted, unwanted extension detected: file1.txt, file2.txt, nested.txt"
	if e.FailMessage != want {
		t.Errorf("FailMessage = %q, want %q", e.FailMessage, want)
	}
	if e.Unwanted != unwanted.StateBlocked {
		t.Errorf("history Unwanted = %d, want blocked", e.Unwanted)
	}

	// A retry is refused with "in progress" while the finalizer still holds
	// the job, which outlasts the history row and the queue entry: try again
	// until that refusal clears.
	retry := func(f func(context.Context, string) error) error {
		var err error
		if !h.WaitUntil(30*time.Second, func() bool {
			err = f(t.Context(), j.ID())
			return err == nil || !strings.Contains(err.Error(), "in progress")
		}) {
			t.Fatal("the finalizer never released the job")
		}
		return err
	}
	// The NZB's own names are clean, so only the entry's Blocked standing can
	// refuse a plain retry.
	if err := retry(h.app.RetryHistoryJob); !errors.Is(err, app.ErrUnwantedRefused) {
		t.Fatalf("plain RetryHistoryJob = %v, want ErrUnwantedRefused", err)
	}
	if err := retry(h.app.RetryHistoryJobAllowingUnwanted); err != nil {
		t.Fatalf("RetryHistoryJobAllowingUnwanted: %v", err)
	}
	if !h.WaitUntil(30*time.Second, func() bool {
		got, err := h.repo.Get(t.Context(), j.ID())
		return err == nil && got.Status == string(constants.StatusCompleted)
	}) {
		got, _ := h.repo.Get(t.Context(), j.ID())
		t.Fatalf("the approved retry did not complete: entry %+v", got)
	}
	if got, _ := h.repo.Get(t.Context(), j.ID()); got.Unwanted != unwanted.StateApproved {
		t.Errorf("after the approved retry history Unwanted = %d, want approved", got.Unwanted)
	}
}

// TestArchivePeek_Pipeline_PauseActionHoldsTheJobUntilApproved drives the
// pause action through the real pipeline: the job is held, blocked and
// paused in the queue, and the user's resume approves it and lets it finish.
func TestArchivePeek_Pipeline_PauseActionHoldsTheJobUntilApproved(t *testing.T) {
	t.Parallel()
	h := archivePeekHarness(t, unwanted.ActionPause)
	raw := h.peekNZB("release.rar", singleRAR(t))
	j := h.addRaw("peek.nzb", raw)

	if !h.WaitUntil(30*time.Second, func() bool {
		row, ok := h.app.Dispatcher().Row(j.ID())
		return ok && row.Header.Unwanted == unwanted.StateBlocked
	}) {
		t.Fatal("the job was never blocked by the downloaded archive")
	}
	if in := j.Intent(); in != job.IntentPause {
		t.Errorf("Intent = %v, want IntentPause", in)
	}
	if err := h.app.Dispatcher().ResumeJob(j.ID()); err == nil {
		t.Error("an application resume of the blocked job was accepted")
	}
	if err := h.app.Dispatcher().ResumeJobByUser(j.ID()); err != nil {
		t.Fatalf("ResumeJobByUser: %v", err)
	}
	e := h.historyEntry(j.ID())
	if e.Status != string(constants.StatusCompleted) || e.Unwanted != unwanted.StateApproved {
		t.Errorf("entry = status %q unwanted %d, want Completed and approved", e.Status, e.Unwanted)
	}
}
