package app

import (
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// resumedPeekNZB renders a two-file NZB: A.bin is one article of size bytes,
// B.bin one of lrArtLen, so B.bin keeps the job from completing.
func resumedPeekNZB(size int) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="iso-8859-1" ?>` + "\n")
	b.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")
	for fi, f := range []struct {
		name string
		n    int
	}{{"A.bin", size}, {"B.bin", lrArtLen}} {
		fmt.Fprintf(&b, `<file poster="p@t" date="1700000000" subject="&quot;%s&quot; yEnc (1/1)">`+"\n", f.name)
		b.WriteString("<groups><group>alt.bin.test</group></groups>\n<segments>\n")
		fmt.Fprintf(&b, `<segment bytes="%d" number="1">f%da1@t</segment>`+"\n", f.n, fi)
		b.WriteString("</segments>\n</file>\n")
	}
	b.WriteString("</nzb>\n")
	return []byte(b.String())
}

// TestHydrate_RealPeekBlocksTheJobFromInsideTheTicksHydration runs the real
// peekResumedFile inside a real Hydrate, called by the dispatcher's tick: A.bin
// is a par2 file declaring two unwanted names, recorded before a restart.
// BlockUnwanted, and under the pause action its yieldPaused, run from inside
// that hydration; neither may wait on it. Under the fail action the history
// entry keeps the flagged names, which the hydration carries to the Resumed
// completion (FileComplete.FailMsg).
func TestHydrate_RealPeekBlocksTheJobFromInsideTheTicksHydration(t *testing.T) {
	t.Parallel()
	names := []string{"a.exe", "b.scr"}
	data := par2Declaring(names...)
	for _, c := range []struct {
		name   string
		action unwanted.Action
	}{
		{"pause", unwanted.ActionPause},
		{"fail", unwanted.ActionFail},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env := newLREnv(t)
			a1 := env.newApp(t)
			raw := resumedPeekNZB(len(data))
			parsed, err := nzb.Parse(strings.NewReader(string(raw)))
			if err != nil {
				t.Fatalf("nzb.Parse: %v", err)
			}
			j, hdr, err := BuildIngestJob(a1.config, parsed, "peek.nzb", types.FetchOptions{NzbName: "peek"}, nil)
			if err != nil {
				t.Fatalf("BuildIngestJob: %v", err)
			}
			if err := a1.AddJob(t.Context(), j, hdr, raw, true); err != nil {
				t.Fatalf("AddJob: %v", err)
			}
			if err := os.MkdirAll(filepath.Dir(env.filePath(j)), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(env.filePath(j), data, 0o600); err != nil {
				t.Fatal(err)
			}
			err = a1.st.ApplyRecord(t.Context(), []durability.RecordBatch{{
				JobID: j.ID(),
				Files: []durability.FileState{{FileIdx: 0, Filename: "A.bin"}},
				Rows:  []durability.WrittenRow{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: int64(len(data)), CRC32: crc32.ChecksumIEEE(data)}},
			}})
			if err != nil {
				t.Fatalf("ApplyRecord: %v", err)
			}

			a2 := env.newApp(t, func(a *Application) {
				a.config.With(func(cfg *config.Config) {
					cfg.Downloads.UnwantedExtensions = []string{"exe", "scr"}
					cfg.Downloads.UnwantedExtensionsMode = unwanted.ModeBlacklist
					cfg.Downloads.ActionOnUnwantedExtensions = c.action
				})
			})
			a2.start(t)
			id := j.ID()
			j2 := a2.registered(t, id)

			switch c.action {
			case unwanted.ActionPause:
				// The tick hydrates the job and marks file A complete in the
				// same call, after the peek: seeing both is the bounded proof
				// that the hydration returned.
				lrWaitFor(t, "the tick's hydration to finish", func() bool {
					p := j2.Progress()
					return j2.Resident() && p != nil && p.FileComplete(0)
				})
				if st, ok := a2.dispatcher.UnwantedState(id); !ok || st != unwanted.StateBlocked {
					t.Fatalf("Unwanted = %d (registered %v), want blocked", st, ok)
				}
				lrWaitFor(t, "the job to be paused", func() bool { return j2.Intent() == job.IntentPause })
			case unwanted.ActionFail:
				// The filed job leaves the queue, so the hydration's return is
				// proved by the filing itself, which only the completion the
				// hydration queued can make.
				var e *history.Entry
				lrWaitFor(t, "the failed job's history entry", func() bool {
					e, err = a2.repo.Get(t.Context(), id)
					return err == nil && e != nil
				})
				if e.Status != string(constants.StatusFailed) {
					t.Errorf("Status = %q, want Failed", e.Status)
				}
				if want := unwantedFailMessage(names); e.FailMessage != want {
					t.Errorf("FailMessage = %q, want %q: the flagged names must reach the history entry", e.FailMessage, want)
				}
			}
		})
	}
}
