package job

import (
	"testing"
	"time"
)

// TestDownloadBegun pins what Job.DownloadBegun reads: the restored stamp
// before hydration, then whether any file has a resolved filename — set live
// (RecordFileFilename) or restored (InstallFileVerification), and kept by
// ResetForRetry — and not an attempt, a failed article, or the start stamp.
func TestDownloadBegun(t *testing.T) {
	manifest := func() *Manifest {
		return NewManifest([]JobFile{
			{Subject: "f", Bytes: 200, Articles: []JobArticle{
				{ID: "<a@x>", Bytes: 100, Number: 1}, {ID: "<b@x>", Bytes: 100, Number: 2},
			}},
			{Subject: "g", Bytes: 100, Articles: []JobArticle{{ID: "<c@x>", Bytes: 100, Number: 1}}},
		})
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

	setName := func(t *testing.T, j *Job, fi int) {
		t.Helper()
		if err := j.RecordFileFilename(fi, j.Name(), "on-disk.bin"); err != nil {
			t.Fatalf("RecordFileFilename: %v", err)
		}
	}
	for name, tc := range map[string]struct {
		prep func(*testing.T, *Job)
		want bool
	}{
		"untouched":    {func(*testing.T, *Job) {}, false},
		"attempt only": {func(_ *testing.T, j *Job) { _ = j.BeginAttempt(stamp) }, false},
		// The pipeline cannot stamp a job without registering a file; the
		// predicate reads the registration, not the stamp.
		"stamp only":              {func(_ *testing.T, j *Job) { _ = j.MarkJobStarted(stamp) }, false},
		"failed, not registered":  {func(_ *testing.T, j *Job) { _ = j.MarkArticleFailed(0) }, false},
		"second file's filename":  {func(t *testing.T, j *Job) { setName(t, j, 1) }, true},
		"failed, then registered": {func(t *testing.T, j *Job) { _ = j.MarkArticleFailed(0); setName(t, j, 0) }, true},
		"retried": {func(t *testing.T, j *Job) {
			setName(t, j, 0)
			markWritten(t, j, 0)
			_ = j.MarkJobStarted(stamp)
			j.ResetForRetry()
		}, true},
		"restored filename": {func(t *testing.T, j *Job) {
			if _, err := j.InstallFileVerification(FileVerification{FileIdx: 0, Filename: "on-disk.bin"}); err != nil {
				t.Fatalf("InstallFileVerification: %v", err)
			}
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			j := New(name, name, Policy{})
			if err := j.AttachContent(manifest()); err != nil {
				t.Fatalf("AttachContent: %v", err)
			}
			tc.prep(t, j)
			if got := j.DownloadBegun(); got != tc.want {
				t.Errorf("DownloadBegun = %v, want %v", got, tc.want)
			}
		})
	}
}
