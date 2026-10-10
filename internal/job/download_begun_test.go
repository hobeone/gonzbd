package job

import (
	"testing"
	"time"
)

// TestDownloadBegun pins what Job.DownloadBegun reads: the restored stamp
// before hydration, then the live stamp or any done article, and not an
// attempt, a failed article, or an untouched job.
func TestDownloadBegun(t *testing.T) {
	manifest := func() *Manifest {
		return NewManifest([]JobFile{{Subject: "f", Bytes: 200, Articles: []JobArticle{
			{ID: "<a@x>", Bytes: 100, Number: 1}, {ID: "<b@x>", Bytes: 100, Number: 2},
		}}})
	}
	stamp := time.Unix(1700000100, 0).UTC()

	restored := New("r", "r", Policy{})
	if restored.DownloadBegun() {
		t.Error("a fresh job with no progress reports DownloadBegun")
	}
	restored.RestoreProgressState("", stamp, time.Time{}, false)
	if !restored.DownloadBegun() {
		t.Error("a restored job with a start stamp and no progress does not report DownloadBegun")
	}

	for name, tc := range map[string]struct {
		prep func(*Job)
		want bool
	}{
		"untouched":      {func(*Job) {}, false},
		"attempt only":   {func(j *Job) { _ = j.BeginAttempt(stamp) }, false},
		"failed article": {func(j *Job) { _ = j.MarkArticleFailed(0) }, false},
		"stamp":          {func(j *Job) { _ = j.MarkJobStarted(stamp) }, true},
		"done article":   {func(j *Job) { markWritten(t, j, 0) }, true},
	} {
		j := New(name, name, Policy{})
		if err := j.AttachContent(manifest()); err != nil {
			t.Fatalf("%s: AttachContent: %v", name, err)
		}
		tc.prep(j)
		if got := j.DownloadBegun(); got != tc.want {
			t.Errorf("%s: DownloadBegun = %v, want %v", name, got, tc.want)
		}
	}
}
