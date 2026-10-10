package job

import (
	"errors"
	"hash/crc32"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// TestInstallFileVerification_SettlesAFinishedFileFromItsRows pins one call's
// whole post-state for a file the verifier finished by path: the filename and
// the asked-for policy are restored, every row's article is Done, the CRC is
// settled from those rows and the rows are released. The file is not marked
// Complete: that is the caller's second step.
func TestInstallFileVerification_SettlesAFinishedFileFromItsRows(t *testing.T) {
	t.Parallel()
	data := chainData()
	j := verifiedTestJob(t)
	dropped, err := j.InstallFileVerification(FileVerification{
		FileIdx: 0, Filename: "a.final", RestorePolicy: true, Policy: FetchNever,
		Rows: chainRows(data), Settle: true,
	})
	if err != nil || dropped != 0 {
		t.Fatalf("InstallFileVerification = %d, %v; want 0, nil", dropped, err)
	}
	p := j.Progress()
	if got := p.FileFilename(0); got != "a.final" {
		t.Errorf("filename = %q, want the recorded a.final", got)
	}
	if got := p.FileFetchPolicy(0); got != FetchNever {
		t.Errorf("fetch policy = %v, want the recorded FetchNever", got)
	}
	for art := range 4 {
		if !p.ArticleDone(art) || p.ArticleFailed(art) {
			t.Errorf("article %d: done=%v failed=%v, want done and not failed", art, p.ArticleDone(art), p.ArticleFailed(art))
		}
	}
	if got, want := p.FileAssembledCRC32(0), crc32.ChecksumIEEE(data); got != want {
		t.Errorf("CRC = %08x, want %08x settled from the installed rows", got, want)
	}
	if got := j.FileRows(0); got != nil {
		t.Errorf("FileRows(0) = %+v after the settle, want none", got)
	}
	if p.FileComplete(0) {
		t.Error("the file is Complete; marking it is the caller's step after the peek")
	}
}

// TestInstallFileVerification_KeepsTheDerivedPolicyUnlessAsked pins that the
// recorded policy is applied only on request: a retry keeps what its rebuilt
// job derived.
func TestInstallFileVerification_KeepsTheDerivedPolicyUnlessAsked(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	if _, err := j.InstallFileVerification(FileVerification{FileIdx: 1, Policy: FetchNever}); err != nil {
		t.Fatalf("InstallFileVerification: %v", err)
	}
	if got := j.FileFetchPolicy(1); got != FetchAlways {
		t.Errorf("fetch policy = %v, want the derived FetchAlways: the caller did not ask for the record's", got)
	}
}

// TestInstallFileVerification_FailsTheIntersectionLosers pins that a loser of
// an intersection is failed and leaves the finished file with no CRC.
func TestInstallFileVerification_FailsTheIntersectionLosers(t *testing.T) {
	t.Parallel()
	rows := chainRows(chainData())
	j := verifiedTestJob(t)
	if _, err := j.InstallFileVerification(FileVerification{
		FileIdx: 0, Rows: []durability.WrittenRow{rows[0], rows[1], rows[3]}, Failed: []int32{2}, Settle: true,
	}); err != nil {
		t.Fatalf("InstallFileVerification: %v", err)
	}
	p := j.Progress()
	if !p.ArticleFailed(2) {
		t.Error("the intersection's loser is not failed")
	}
	if got := p.FileAssembledCRC32(0); got != 0 {
		t.Errorf("CRC = %08x for a file with a failed article, want 0", got)
	}
}

// TestInstallFileVerification_ChangesNothingWhenItRefuses pins that the call
// is checked before any of it is applied: an out-of-range file or a
// non-resident job returns an error and leaves the job as it was.
func TestInstallFileVerification_ChangesNothingWhenItRefuses(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	_, err := j.InstallFileVerification(FileVerification{
		FileIdx: 2, Failed: []int32{0}, Rows: chainRows(chainData()), Settle: true,
	})
	if err == nil {
		t.Fatal("InstallFileVerification accepted a file index past the manifest")
	}
	p := j.Progress()
	for art := range 5 {
		if p.ArticleDone(art) {
			t.Errorf("article %d is Done after a refused install", art)
		}
	}

	cold := New("cold", "cold", Policy{})
	if _, err := cold.InstallFileVerification(FileVerification{FileIdx: 0}); !errors.Is(err, ErrNotResident) {
		t.Errorf("InstallFileVerification on a non-resident job = %v, want ErrNotResident", err)
	}
}

// installSnapshot is what TestInstallFileVerification_NoReaderSeesHalfOfIt
// compares: the parts of one file's state the install changes.
type installSnapshot struct {
	filename string
	policy   FetchPolicy
	done     int
	failed   int
	crc      uint32
}

func snapshotFile0(j *Job) installSnapshot {
	p := j.Progress()
	s := installSnapshot{filename: p.FileFilename(0), policy: p.FileFetchPolicy(0), crc: p.FileAssembledCRC32(0)}
	for art := range 4 {
		if p.ArticleFailed(art) {
			s.failed++
		} else if p.ArticleDone(art) {
			s.done++
		}
	}
	return s
}

// TestInstallFileVerification_NoReaderSeesHalfOfIt reads the job while one
// install runs: every snapshot is either the state before the install or the
// state after it, never one with the rows installed and the CRC not yet
// settled, or the policy restored and the rows not yet installed.
func TestInstallFileVerification_NoReaderSeesHalfOfIt(t *testing.T) {
	t.Parallel()
	data := chainData()
	rows := chainRows(data)
	for range 50 {
		j := verifiedTestJob(t)
		before := snapshotFile0(j)
		v := FileVerification{
			FileIdx: 0, Filename: "a.final", RestorePolicy: true, Policy: FetchIfNeeded,
			Rows: rows[:3], Failed: []int32{3}, Settle: true,
		}
		after := installSnapshot{filename: "a.final", policy: FetchIfNeeded, done: 3, failed: 1, crc: 0}

		stop := make(chan struct{})
		var wg sync.WaitGroup
		var seen []installSnapshot
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				seen = append(seen, snapshotFile0(j))
			}
		})
		if _, err := j.InstallFileVerification(v); err != nil {
			t.Fatalf("InstallFileVerification: %v", err)
		}
		close(stop)
		wg.Wait()
		seen = append(seen, snapshotFile0(j))
		for _, s := range seen {
			if s != before && s != after {
				t.Fatalf("a reader saw %+v, which is neither the state before (%+v) nor after (%+v) the install", s, before, after)
			}
		}
		if got := seen[len(seen)-1]; got != after {
			t.Fatalf("state after the install = %+v, want %+v", got, after)
		}
		if j.FileRows(0) != nil {
			t.Fatal("the settled file's rows are still resident")
		}
	}
}
