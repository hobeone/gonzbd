package assembler

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestOpenTargetFile_RefusesToWriteOutOfTheJobDirectory pins the writer's
// os.Root open: a symlink planted in the job directory under the target's
// name, or a name that climbs out with "..", is an open error. The file
// outside is neither created nor written, and the article takes the existing
// failed-open path: a routed "open" fault and the article handed back as
// unwritten, with nothing left in the open map.
func TestOpenTargetFile_RefusesToWriteOutOfTheJobDirectory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// plant prepares jobDir and returns the name to open and the outside
		// path a plain-path open would write.
		plant func(t *testing.T, jobDir, outside string) (name, target string)
		// existing is the outside file's content before the write, or nil when
		// the plain-path open would create it.
		existing []byte
	}{
		{"a symlink to a missing file outside", func(t *testing.T, jobDir, outside string) (string, string) {
			target := filepath.Join(outside, "created.bin")
			if err := os.Symlink(target, filepath.Join(jobDir, "f.bin")); err != nil {
				t.Fatal(err)
			}
			return "f.bin", target
		}, nil},
		{"a symlink to an existing file outside", func(t *testing.T, jobDir, outside string) (string, string) {
			target := filepath.Join(outside, "victim.bin")
			if err := os.WriteFile(target, []byte("ORIGINAL"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(jobDir, "f.bin")); err != nil {
				t.Fatal(err)
			}
			return "f.bin", target
		}, []byte("ORIGINAL")},
		{"a name that climbs out", func(t *testing.T, jobDir, _ string) (string, string) {
			return filepath.Join("..", "escaped.bin"), filepath.Join(filepath.Dir(jobDir), "escaped.bin")
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			jobDir := filepath.Join(base, "job")
			if err := os.Mkdir(jobDir, 0o750); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			name, target := tc.plant(t, jobDir, outside)

			a := newHelperAssembler()
			a.opts.FileInfo = func(string, int) (FileInfo, error) {
				return FileInfo{Dir: jobDir, Name: name, TotalParts: 1}, nil
			}
			var faults []*storagefault.Fault
			a.opts.OnWriteFault = func(_ string, _ int, f *storagefault.Fault) { faults = append(faults, f) }
			var unwritten []int32
			a.opts.OnArticlesUnwritten = func(_ string, _ int, idx []int32) { unwritten = append(unwritten, idx...) }
			a.putBuffer = func([]byte) {}

			open := map[fileKey]*openFile{}
			a.processRequest(WriteRequest{
				JobID: "job1", FileIdx: 0, ArtIdx: 3, MessageID: "m1",
				Offset: 0, Data: []byte("PAYLOAD!"),
			}, open, map[fileKey]struct{}{})
			for _, f := range open {
				_ = f.w.Close()
			}

			got, err := os.ReadFile(target)
			switch {
			case tc.existing == nil && !errors.Is(err, fs.ErrNotExist):
				t.Errorf("outside file %s: read = %q, %v; want it never created", target, got, err)
			case tc.existing != nil && string(got) != string(tc.existing):
				t.Errorf("outside file %s = %q, %v; want it untouched at %q", target, got, err, tc.existing)
			}
			if len(faults) != 1 || faults[0].Op != "open" {
				t.Fatalf("routed faults = %v, want one open fault", faults)
			}
			if len(unwritten) != 1 || unwritten[0] != 3 {
				t.Errorf("unwritten = %v, want [3]: the refused article must be handed back", unwritten)
			}
			if len(open) != 0 {
				t.Errorf("open map has %d files after a refused open, want 0", len(open))
			}
		})
	}
}
