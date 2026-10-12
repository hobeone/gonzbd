package postproc

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemovePendingSidecar_LogsOnlyARealFailure pins that a sidecar the
// removal could not unlink is logged, and that one already gone is not.
func TestRemovePendingSidecar_LogsOnlyARealFailure(t *testing.T) {
	t.Parallel()
	t.Run("refused", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// A non-empty directory under the sidecar's name refuses the unlink.
		if err := os.MkdirAll(filepath.Join(dir, pendingDeletionsFile, "child"), 0o700); err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		removePendingSidecar(slog.New(slog.NewTextHandler(&logs, nil)), dir)
		if !strings.Contains(logs.String(), "level=WARN") {
			t.Errorf("a refused sidecar removal was not logged at Warn; log:\n%s", logs.String())
		}
	})
	t.Run("already gone", func(t *testing.T) {
		t.Parallel()
		var logs bytes.Buffer
		removePendingSidecar(slog.New(slog.NewTextHandler(&logs, nil)), t.TempDir())
		if logs.Len() != 0 {
			t.Errorf("a sidecar already gone was logged; log:\n%s", logs.String())
		}
	})
	t.Run("nil logger", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, pendingDeletionsFile, "child"), 0o700); err != nil {
			t.Fatal(err)
		}
		removePendingSidecar(nil, dir) // must not panic
	})
}
