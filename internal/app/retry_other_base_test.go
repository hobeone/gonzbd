package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/postproc"
)

func TestCheckRecordedUnderCurrentBase(t *testing.T) {
	t.Parallel()
	base, other, complete := t.TempDir(), t.TempDir(), t.TempDir()
	failed := postproc.FailedDir(filepath.Join(base, "job"))

	for _, tc := range []struct {
		name     string
		recorded string
		download string
		refused  bool
	}{
		{"same base", failed, base, false},
		{"same base with a trailing slash", failed, base + "/", false},
		{"same base with doubled slashes", failed, base + "//", false},
		{"same base spelled with a dot segment", failed, base + "/./", false},
		{"no recorded path", "", base, false},
		{"entry under complete_dir", filepath.Join(complete, "cat", "job"), base, false},
		{"entry under an earlier base", postproc.FailedDir(filepath.Join(other, "job")), base, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkRecordedUnderCurrentBase(tc.recorded, tc.download, complete)
			if !tc.refused {
				if err != nil {
					t.Fatalf("checkRecordedUnderCurrentBase = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, errRetryDirConflict) || !strings.Contains(err.Error(), "not under the current download_dir") {
				t.Fatalf("checkRecordedUnderCurrentBase = %v, want errRetryDirConflict naming the recorded path", err)
			}
		})
	}
}
