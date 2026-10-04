package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/postproc"
)

func TestRestoreFailedDir_RefusesAnEntryRecordedUnderAnotherBase(t *testing.T) {
	t.Parallel()
	oldBase, newBase := t.TempDir(), t.TempDir()
	recorded := postproc.FailedDir(filepath.Join(oldBase, "job"))
	if err := os.MkdirAll(recorded, 0o750); err != nil {
		t.Fatal(err)
	}

	from, err := restoreFailedDir(recorded, newBase, "job", func(string) bool { return false })

	if !errors.Is(err, errRetryDirConflict) || !strings.Contains(err.Error(), "download_dir changed") {
		t.Fatalf("restoreFailedDir error = %v, want errRetryDirConflict naming the download_dir change", err)
	}
	if from != "" {
		t.Errorf("restoreFailedDir moved from %q, want nothing moved", from)
	}
}
