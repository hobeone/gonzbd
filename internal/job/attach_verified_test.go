package job

import (
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// unattachedVerifiedJob is verifiedTestJob's two files with the table attached
// from a seed and no manifest, as a restart's restore pass leaves a job: every
// file unverified. It also returns the manifest to attach.
func unattachedVerifiedJob(t *testing.T) (*Job, *Manifest) {
	t.Helper()
	src := verifiedTestJob(t)
	m, err := src.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	j := New("verified", "verified", Policy{})
	if err := j.AttachFiles(m.Files(), []FileSeed{{}, {}}); err != nil {
		t.Fatalf("AttachFiles: %v", err)
	}
	return j, m
}

// AttachArticles installs each verification as it attaches: when it returns,
// the job is resident and every installed row's article is Done, and a
// verification that cannot be installed is reported in its own slot without
// stopping the others.
func TestAttachArticles_InstallsEachVerification(t *testing.T) {
	t.Parallel()
	j, m := unattachedVerifiedJob(t)
	rows := chainRows(chainData())[:2]
	rows = append(rows, durability.WrittenRow{FileIdx: 0, ArtIdx: 4, Offset: 0, Length: 100}) // article 4 is file 1's
	installs, err := j.AttachArticles(m, []FileVerification{
		{FileIdx: 0, Rows: rows},
		{FileIdx: 9},
		{FileIdx: 1},
	})
	if err != nil {
		t.Fatalf("AttachArticles: %v", err)
	}
	if !j.Resident() {
		t.Fatal("the job is not resident after AttachArticles")
	}
	if len(installs) != 3 {
		t.Fatalf("got %d install outcomes, want one per verification (3)", len(installs))
	}
	if installs[0].Err != nil || installs[0].Dropped != 1 {
		t.Errorf("file 0 install = %+v, want no error and the misplaced row dropped", installs[0])
	}
	if installs[1].Err == nil {
		t.Error("a verification naming no file installed without an error")
	}
	if installs[2].Err != nil {
		t.Errorf("file 1 install = %v after file 9's failed; want each installed whatever became of the one before", installs[2].Err)
	}
	p := j.Progress()
	for art := range 4 {
		if got, want := p.ArticleDone(art), art < 2; got != want {
			t.Errorf("article %d Done = %v, want %v", art, got, want)
		}
	}
	if !p.FileVerified(0) || !p.FileVerified(1) {
		t.Errorf("files verified = %v, %v; want both", p.FileVerified(0), p.FileVerified(1))
	}
}

// installFileVerificationLocked refuses, having changed nothing, a job that is
// not resident and a FileIdx that names no file. Its callers hold contentMu,
// and so does this test.
func TestInstallFileVerificationLocked_RefusesWithoutChange(t *testing.T) {
	t.Parallel()
	j, m := unattachedVerifiedJob(t)
	j.contentMu.Lock()
	_, err := j.installFileVerificationLocked(FileVerification{FileIdx: 0, Rows: chainRows(chainData())})
	j.contentMu.Unlock()
	if !errors.Is(err, ErrNotResident) {
		t.Errorf("install on a job with no manifest = %v, want ErrNotResident", err)
	}
	if p := j.Progress(); p.ArticleDone(0) || p.FileVerified(0) {
		t.Errorf("a refused install changed file 0: done=%v verified=%v", p.ArticleDone(0), p.FileVerified(0))
	}

	if _, err := j.AttachArticles(m, nil); err != nil {
		t.Fatalf("AttachArticles: %v", err)
	}
	j.contentMu.Lock()
	_, err = j.installFileVerificationLocked(FileVerification{FileIdx: 2, Filename: "x"})
	j.contentMu.Unlock()
	if err == nil {
		t.Error("install for FileIdx 2 of a two-file job returned no error")
	}
	if p := j.Progress(); p.FileVerified(0) || p.FileVerified(1) {
		t.Errorf("a refused install marked a file verified: %v, %v", p.FileVerified(0), p.FileVerified(1))
	}
}

// A refused attach installs nothing: the job stays unresident and no
// verification's rows are Done.
func TestAttachArticles_RefusedInstallsNothing(t *testing.T) {
	t.Parallel()
	j, m := unattachedVerifiedJob(t)
	other, _ := unattachedVerifiedJob(t)
	om, err := NewManifestFromArticles(other.Files(), []JobArticle{
		{ID: "<a0@x>", Bytes: 100}, {ID: "<a1@x>", Bytes: 100}, {ID: "<a2@x>", Bytes: 100},
		{ID: "<a3@x>", Bytes: 100}, {ID: "<b0@x>", Bytes: 100},
	})
	if err != nil {
		t.Fatalf("NewManifestFromArticles: %v", err)
	}
	installs, err := j.AttachArticles(om, []FileVerification{{FileIdx: 0, Rows: chainRows(chainData())}})
	if err == nil {
		t.Fatal("AttachArticles accepted a manifest built from another job's table")
	}
	if installs != nil {
		t.Errorf("a refused attach reported installs %+v, want none", installs)
	}
	if j.Resident() {
		t.Error("a refused attach left the job resident")
	}
	if p := j.Progress(); p.ArticleDone(0) || p.FileVerified(0) {
		t.Errorf("a refused attach installed file 0: done=%v verified=%v", p.ArticleDone(0), p.FileVerified(0))
	}
	if _, err := j.AttachArticles(m, nil); err != nil {
		t.Fatalf("AttachArticles with the job's own manifest after the refusal: %v", err)
	}
}
