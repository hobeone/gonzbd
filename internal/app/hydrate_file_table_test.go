package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/job"
)

// restartedUnverified queues a two-file job in one process and returns the
// same job as a second process over the same directories restored it, after
// the restore pass: a file table and a seeded, unverified progress record, no
// manifest. The second process is not started, so nothing hydrates the job
// but the test.
func restartedUnverified(t *testing.T, env *lrEnv, name string, nA, nB int) (orig, restored *job.Job, a2 *lrApp) {
	t.Helper()
	a1 := env.newApp(t)
	orig = a1.addJob(t, name, nA, nB)
	a2 = env.newApp(t)
	restored = a2.registered(t, orig.ID())
	if restored == orig {
		t.Fatal("fixture: the second process holds the first process's job instance")
	}
	if err := a2.restoreProgress(t.Context()); err != nil {
		t.Fatalf("restoreProgress: %v", err)
	}
	if restored.Files() == nil || !restored.HasProgress() || restored.AllVerified() || restored.Resident() {
		t.Fatal("fixture: the restore pass did not leave the job with an unverified table and progress and no manifest")
	}
	return orig, restored, a2
}

// The file table the restore pass attaches is rebuilt from the job's job_files
// rows, which ingest wrote from the same table: subjects, sizes and article
// counts survive the restart without the manifest file carrying any of them,
// and the job hydrates over it.
func TestRestoreProgress_BuildsTheFileTableFromJobFiles(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	orig, restored, a2 := restartedUnverified(t, env, "fromrows", 3, 2)

	want, got := orig.Files(), restored.Files()
	if got.NumFiles() != want.NumFiles() {
		t.Fatalf("NumFiles = %d, want %d", got.NumFiles(), want.NumFiles())
	}
	for fi := range want.NumFiles() {
		if got.Subject(fi) != want.Subject(fi) || got.Bytes(fi) != want.Bytes(fi) || got.ArticleCount(fi) != want.ArticleCount(fi) {
			t.Errorf("file %d = {%q %d %d}, want {%q %d %d}", fi,
				got.Subject(fi), got.Bytes(fi), got.ArticleCount(fi),
				want.Subject(fi), want.Bytes(fi), want.ArticleCount(fi))
		}
	}
	if got.Subject(0) == "" {
		t.Error("the rebuilt table has an empty subject: the rows carried no structure")
	}
	if err := a2.residency.Hydrate(t.Context(), restored.ID()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if !restored.Resident() || !restored.AllVerified() || restored.Files() != got {
		t.Errorf("after Hydrate: Resident = %v, AllVerified = %v, same table = %v; want all three",
			restored.Resident(), restored.AllVerified(), restored.Files() == got)
	}
}

// A manifest whose article count disagrees with the rows is refused by
// NewManifestFromArticles. That is a corrupt manifest, which the tick settles
// the job Failed on, and not a residency fault, which it would leave parked.
func TestHydrate_RefusesAManifestWhoseCountDisagreesWithTheRows(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	orig, restored, a2 := restartedUnverified(t, env, "shortmanifest", 3, 2)

	m, err := orig.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	type article struct {
		ID     string `json:"id"`
		Bytes  int    `json:"bytes"`
		Number int    `json:"number"`
	}
	arts := make([]article, 0, m.NumArticles())
	for i := range m.NumArticles() - 1 { // one fewer than the rows describe
		arts = append(arts, article{ID: m.ArticleID(i), Bytes: m.ArticleBytes(i), Number: m.ArticleNumber(i)})
	}
	data, err := json.Marshal(struct {
		Articles []article `json:"articles"`
	}{arts})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(manifestDir(env.admin), orig.ID()+".json.gz")
	if err := fsutil.WriteGzAtomicBytes(path, data); err != nil {
		t.Fatalf("write the short manifest: %v", err)
	}

	err = a2.residency.Hydrate(t.Context(), restored.ID())
	if err == nil {
		t.Fatal("Hydrate attached a manifest with one article fewer than job_files describes")
	}
	if errors.Is(err, dispatch.ErrResidencyFault) {
		t.Errorf("Hydrate = %v, a residency fault; a corrupt manifest is not a fault about the device", err)
	}
	if restored.Resident() || restored.AllVerified() {
		t.Error("a refused hydration left the job resident or verified")
	}
}

// TestHydrate_RefusesAJobWithoutAFileTable pins D18: Hydrate needs the table
// the restore pass or ingest attached, and builds none itself.
func TestHydrate_RefusesAJobWithoutAFileTable(t *testing.T) {
	t.Parallel()
	j := job.New("abc123", "test", job.PolicyFromPP(3))
	r := newAppResidency(func(string) (*job.Job, bool) { return j, true }, t.TempDir(), nil, nil)

	err := r.Hydrate(context.Background(), "abc123")
	if err == nil || !strings.Contains(err.Error(), "no file table attached; the restore pass did not run") {
		t.Fatalf("Hydrate = %v, want the D18 error: no file table attached", err)
	}
	if j.Files() != nil || j.Resident() {
		t.Error("a refused hydration attached content")
	}
}
