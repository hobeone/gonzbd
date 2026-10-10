package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRegisterFile_SuffixesANameAlreadyTakenOnDisk pins the collision rule
// registerFile applies the first time it names a file: a name already held in
// the job directory, by a file or by a symlink (dangling or not), gets a ".1"
// before its extension, and the chosen name is what the writer is handed and
// what the job records. The FileInfo carries the job directory and the bare
// name, which the writer opens through an os.Root.
func TestRegisterFile_SuffixesANameAlreadyTakenOnDisk(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		plant   func(t *testing.T, path string)
		suffix  bool
		jobName string
	}{
		{"a free name is kept", func(*testing.T, string) {}, false, "uniqfree"},
		{"a regular file takes the name", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, true, "uniqfile"},
		{"a dangling symlink takes the name", func(t *testing.T, path string) {
			if err := os.Symlink(filepath.Join(t.TempDir(), "missing.bin"), path); err != nil {
				t.Fatal(err)
			}
		}, true, "uniqlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestApplication(t)
			disp, j := helperJob(t, app, tc.jobName, 1, 1)
			p := helperPipeline(t, disp)
			m := mustManifest(t, j)

			want := p.jobFileLocation(j.Name(), m.FileSubject(0))
			if err := os.MkdirAll(want.Dir, 0o750); err != nil {
				t.Fatal(err)
			}
			tc.plant(t, want.Path())
			if tc.suffix {
				ext := filepath.Ext(want.Name)
				want.Name = strings.TrimSuffix(want.Name, ext) + ".1" + ext
			}

			if err := p.registerFile(j, 0); err != nil {
				t.Fatalf("registerFile: %v", err)
			}
			info, err := p.resolveFileInfo(j.ID(), 0)
			if err != nil {
				t.Fatalf("resolveFileInfo: %v", err)
			}
			if info.Dir != want.Dir || info.Name != want.Name {
				t.Errorf("FileInfo = {Dir: %q, Name: %q}, want {Dir: %q, Name: %q}", info.Dir, info.Name, want.Dir, want.Name)
			}
			if got := j.Progress().FileFilename(0); got != want.Name {
				t.Errorf("recorded filename = %q, want %q", got, want.Name)
			}
		})
	}
}
