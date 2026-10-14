package fsutil

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteAtomic_ChmodFlagGatesThePermission pins writeAtomic's chmod flag:
// set, the published file carries perm; clear, it keeps os.CreateTemp's 0600
// and perm is ignored.
func TestWriteAtomic_ChmodFlagGatesThePermission(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		chmod bool
		want  os.FileMode
	}{
		{"chmod", true, 0o640},
		{"no chmod", false, 0o600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "out")
			err := writeAtomic(path, 0o640, tc.chmod, func(w io.Writer) error {
				_, err := w.Write([]byte("x"))
				return err
			})
			if err != nil {
				t.Fatalf("writeAtomic: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != tc.want {
				t.Errorf("mode = %o, want %o", got, tc.want)
			}
		})
	}
}

func TestIsTempFile_InternalHelpers(t *testing.T) {
	t.Parallel()
	if !isGonzbdTempFile(".gonzbd-tmp-0123456789abcdef") {
		t.Error("isGonzbdTempFile(.gonzbd-tmp-0123456789abcdef) = false, want true")
	}
	if isGonzbdTempFile("part03.rar.par2engine-FW6AYZPLN4JV6JDE.tmp") {
		t.Error("isGonzbdTempFile(par2engine tmp) = true, want false")
	}
	if !isPar2EngineTempFile("part03.rar.par2engine-FW6AYZPLN4JV6JDE.tmp") {
		t.Error("isPar2EngineTempFile(par2engine tmp) = false, want true")
	}
	if isPar2EngineTempFile(".gonzbd-tmp-0123456789abcdef") {
		t.Error("isPar2EngineTempFile(gonzbd tmp) = true, want false")
	}
}
