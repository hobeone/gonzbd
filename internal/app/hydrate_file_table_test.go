package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/job"
)

// restartedUnhydrated queues a two-file job in one process and returns the
// same job as a second process over the same directories restored it: the
// header only, no file table, no progress, no manifest. The second process is
// not started, so nothing hydrates the job but the test.
func restartedUnhydrated(t *testing.T, env *lrEnv, name string, nA, nB int) (orig, restored *job.Job, a2 *lrApp) {
	t.Helper()
	a1 := env.newApp(t)
	orig = a1.addJob(t, name, nA, nB)
	a2 = env.newApp(t)
	restored = a2.registered(t, orig.ID())
	if restored == orig {
		t.Fatal("fixture: the second process holds the first process's job instance")
	}
	if restored.Files() != nil || restored.HasProgress() || restored.Resident() {
		t.Fatal("fixture: a restored job already carries a file table, progress or a manifest")
	}
	return orig, restored, a2
}

// The file table a restored job hydrates with is rebuilt from its job_files
// rows, which ingest wrote from the same table: subjects, sizes and article
// counts survive the restart without the manifest file carrying any of them.
func TestHydrate_BuildsTheFileTableFromJobFilesBeforeVerifying(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	orig, restored, a2 := restartedUnhydrated(t, env, "fromrows", 3, 2)

	if err := a2.residency.Hydrate(t.Context(), restored.ID()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if !restored.Resident() || !restored.HasProgress() {
		t.Fatalf("after Hydrate: Resident = %v, HasProgress = %v, want both", restored.Resident(), restored.HasProgress())
	}
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
}

// A manifest whose article count disagrees with the rows is refused by
// NewManifestFromArticles. That is a corrupt manifest, which the tick settles
// the job Failed on, and not a residency fault, which it would leave parked.
func TestHydrate_RefusesAManifestWhoseCountDisagreesWithTheRows(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	orig, restored, a2 := restartedUnhydrated(t, env, "shortmanifest", 3, 2)

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
	if restored.Resident() || restored.Files() != nil {
		t.Error("a refused hydration left the job resident or with a file table")
	}
}

// Without a record store there is nothing to build a table from, so a job
// that has none attached cannot hydrate.
func TestHydrate_WithoutAStoreNeedsAnAttachedFileTable(t *testing.T) {
	t.Parallel()
	j := job.New("abc123", "test", job.PolicyFromPP(3))
	r := newAppResidency(func(string) (*job.Job, bool) { return j, true }, t.TempDir(), nil, nil)

	err := r.Hydrate(context.Background(), "abc123")
	if err == nil || !strings.Contains(err.Error(), "no record store") {
		t.Fatalf("Hydrate = %v, want an error saying there is no record store", err)
	}
	if j.Files() != nil {
		t.Error("a refused hydration attached a file table")
	}
}

func TestFileTableFromRows_RefusesAGapAndADuplicate(t *testing.T) {
	t.Parallel()
	row := func(idx int, subject string) durability.FileRow {
		return durability.FileRow{FileIndex: idx, Subject: subject, Bytes: int64(idx+1) * 100, ArticleCount: idx + 1}
	}
	for name, rows := range map[string][]durability.FileRow{
		"a gap":       {row(0, "a"), row(2, "c")},
		"a duplicate": {row(0, "a"), row(1, "b"), row(1, "b")},
		"no first":    {row(1, "b")},
	} {
		if ft, err := fileTableFromRows(rows); err == nil {
			t.Errorf("fileTableFromRows(%s) = %d files, nil; want an error", name, ft.NumFiles())
		}
	}

	rows := []durability.FileRow{row(1, "b"), row(0, "a")}
	ft, err := fileTableFromRows(rows)
	if err != nil {
		t.Fatalf("fileTableFromRows(out of order) = %v, want the rows sorted", err)
	}
	if ft.Subject(0) != "a" || ft.Subject(1) != "b" || ft.ArticleCount(1) != 2 || ft.Bytes(0) != 100 {
		t.Errorf("table = {%q %q}, want rows ordered by FileIndex with their figures", ft.Subject(0), ft.Subject(1))
	}
	if rows[0].FileIndex != 1 {
		t.Error("fileTableFromRows reordered the caller's slice")
	}

	empty, err := fileTableFromRows(nil)
	if err != nil || empty.NumFiles() != 0 {
		t.Errorf("fileTableFromRows(nil) = %v files, %v; want a table of no files", empty, err)
	}
}
