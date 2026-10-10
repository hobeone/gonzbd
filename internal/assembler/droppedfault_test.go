package assembler

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestProcessRequest_RoutesAFailedOpen pins finding 11.
//
// processRequest logged openTargetFile's classified fault and returned. The
// file is never inserted into open, so no later fsync or close ever sees it —
// the fault had no path out of the assembler at all. The pipeline saw WriteArticle return nil, so the
// article stayed Emitted and was permanently skipped. A persistent EACCES or
// EROFS on the download directory left the job at N% with no reason attached,
// which is the outcome openTargetFile's own doc says returning a fault
// prevents.
func TestProcessRequest_RoutesAFailedOpen(t *testing.T) {
	dir := t.TempDir()
	// A resolver that always fails: the file can never be opened, so nothing
	// is ever inserted into the worker's open map.
	opts := makeOpts(dir, map[string]FileInfo{})
	var faults []*storagefault.Fault
	var gotArt int32 = -1
	opts.OnWriteFault = func(_ string, _ int, f *storagefault.Fault) {
		faults = append(faults, f)
	}
	opts.OnArticlesUnwritten = func(_ string, _ int, artIdxs []int32) {
		if len(artIdxs) > 0 {
			gotArt = artIdxs[0]
		}
	}

	a := startAssembler(t, opts)
	if err := writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 9, MessageID: "m1",
		Offset: 0, Data: []byte("AAAA"),
	}); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if len(faults) == 0 {
		t.Fatal("a file that could not be opened routed no fault. It is never in the " +
			"open set, so no later fsync or close can ever surface it, and the article " +
			"stays Emitted and is never re-dispatched")
	}
	if gotArt != 9 {
		t.Errorf("routed article %d, want 9 — without it the owner cannot clear the "+
			"Emitted bit and the article is stranded", gotArt)
	}
}

// TestProcessRequest_FailedOpenReleasesTheBufferOnce pins #574.
//
// openTargetFile released req.Data on each of its three failure returns, and
// processRequest released it again on the error it got back. Two releases of
// one slice put one backing array into decoder's pool twice, so two later
// GetBuffer calls handed it to two connWorkers, which decoded into it
// concurrently — the race #574 reported from decoder.sub42Span, whose stacks
// named the victims rather than this site.
//
// One case per failure return, because each carried its own release: a fix to
// one would leave the other two pinned by nothing.
func TestProcessRequest_FailedOpenReleasesTheBufferOnce(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "plain-file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	isADir := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(isADir, 0o750); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		resolver func(string, int) (FileInfo, error)
	}{
		{"resolve fails", func(string, int) (FileInfo, error) {
			return FileInfo{}, os.ErrNotExist
		}},
		{"mkdir fails", func(string, int) (FileInfo, error) {
			// The parent is a regular file, so MkdirAll reports ENOTDIR.
			return FileInfo{Path: filepath.Join(notADir, "sub", "f.bin")}, nil
		}},
		{"open fails", func(string, int) (FileInfo, error) {
			// The target is a directory, so OpenFile(O_WRONLY) reports EISDIR.
			return FileInfo{Path: isADir}, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newHelperAssembler()
			a.opts.FileInfo = tc.resolver
			var faults int
			a.opts.OnWriteFault = func(string, int, *storagefault.Fault) { faults++ }
			a.opts.OnArticlesUnwritten = func(string, int, []int32) {}
			var released [][]byte
			a.putBuffer = func(b []byte) { released = append(released, b) }

			data := []byte("AAAA")
			a.processRequest(WriteRequest{
				JobID: "job1", FileIdx: 0, ArtIdx: 9, MessageID: "m1",
				Offset: 0, Data: data,
			}, map[fileKey]*openFile{}, map[fileKey]struct{}{})

			// Guards the fixture: without a routed fault the open did not
			// fail, and a count of one would prove nothing about this path.
			if faults != 1 {
				t.Fatalf("routed %d faults, want 1; the open did not fail the way this case needs", faults)
			}
			switch {
			case len(released) == 0:
				t.Fatal("released the buffer 0 times, want exactly 1; nothing else " +
					"owns it after a failed open, so the pool leaks one buffer per " +
					"article the file refuses")
			case len(released) > 1:
				t.Fatalf("released the buffer %d times, want exactly 1; a second release "+
					"puts one backing array in decoder's pool twice, and two connWorkers "+
					"then decode into it concurrently (#574)", len(released))
			}
			if &released[0][:1][0] != &data[:1][0] {
				t.Errorf("released a different buffer than the request carried")
			}
		})
	}
}
