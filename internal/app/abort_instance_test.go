package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// TestAbort_YieldsAnInstancePostProcessingDoesNotHold: post-processing still
// running an earlier instance of a job must not make the abort of a later
// instance under the same ID leave that instance's launch claim to
// post-processing. Post-processing releases only the instance it holds, so
// nothing would release the later one's, and removing it would wait out its
// budget.
func TestAbort_YieldsAnInstancePostProcessingDoesNotHold(t *testing.T) {
	t.Parallel()
	stage := gatedStage{entered: make(chan string, 1), finish: make(chan struct{})}
	application, j, _ := heldRepairingApp(t, stage)
	id := j.ID()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stale.bin"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := job.New(id, "stale", job.Policy{})
	application.postProcessor.Process(&postproc.Job{Job: stale, DownloadDir: dir})
	awaitStage(t, stage.entered)
	t.Cleanup(func() { close(stage.finish) })

	if err := application.dispatcher.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := application.dispatcher.Remove(ctx, id); err != nil {
		t.Fatalf("Remove: %v; the abort left the launched instance's claim to post-processing, which holds only an earlier instance", err)
	}
}
