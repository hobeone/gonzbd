package app

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// downloads.min_free_space reaches the unpack stage, from the config at
// construction and from SetMinFreeSpace on a change (#901).
func TestMinFreeSpace_ReachesUnpackStage(t *testing.T) {
	t.Parallel()

	const noRoom = int64(1) << 62
	cfg := convertConfig(Config{DownloadDir: t.TempDir(), CompleteDir: t.TempDir()})
	cfg.With(func(c *config.Config) {
		c.PostProc.EnableTar = true
		c.Downloads.MinFreeSpace = config.ByteSize(noRoom)
	})
	built, err := buildStages(cfg, "", discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}

	// unpackTar runs the unpack stage on a job holding one small tar and
	// reports whether the extraction was refused for want of space.
	unpackTar := func(id string) bool {
		t.Helper()
		dir := t.TempDir()
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := tw.WriteHeader(&tar.Header{Name: "payload.txt", Mode: 0o644, Size: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "set.tar"), buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		j := &postproc.Job{Job: job.New(id, "minfree", job.PolicyFromPP(3)), DownloadDir: dir}
		err := built.Unpack.Run(t.Context(), j)
		if err != nil && !errors.Is(err, unpack.ErrInsufficientSpace) {
			t.Fatalf("Run: %v", err)
		}
		return errors.Is(err, unpack.ErrInsufficientSpace)
	}

	if !unpackTar("minfreebuild0001") {
		t.Error("min_free_space from the config at construction did not reach the unpack stage")
	}
	app := &Application{config: cfg, stages: built}
	app.SetMinFreeSpace(0)
	if unpackTar("minfreeset000001") {
		t.Error("SetMinFreeSpace(0) did not reach the unpack stage")
	}
}
