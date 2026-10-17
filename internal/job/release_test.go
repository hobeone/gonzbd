package job

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

type heldVol struct {
	name  string
	bytes int64
}

// releaseJob builds a job with one content file (index 0) and the given
// recovery volumes (indices 1..), each held as FetchIfNeeded.
func releaseJob(t *testing.T, vols []heldVol) *Job {
	t.Helper()
	files := make([]JobFile, 0, 1+len(vols))
	files = append(files, JobFile{Subject: "data.rar", Bytes: 100, Articles: []JobArticle{{ID: "<d@x>", Bytes: 100, Number: 1}}})
	for i, v := range vols {
		files = append(files, JobFile{
			Subject: v.name, Bytes: v.bytes, IsPar2Recovery: true, Deferred: true,
			Articles: []JobArticle{{ID: fmt.Sprintf("<v%d@x>", i), Bytes: int(v.bytes), Number: 1}},
		})
	}
	j := New("rel", "rel", Policy{})
	if err := j.AttachContent(NewManifest(files)); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	for i := range vols {
		if err := j.SetFileFetchPolicy(i+1, FetchIfNeeded); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
	}
	return j
}

func standardVols() []heldVol {
	return []heldVol{
		{"x.vol00+01.par2", 1000},
		{"x.vol01+02.par2", 2000},
		{"x.vol03+04.par2", 4000},
		{"x.vol07+08.par2", 8000},
		{"x.vol15+16.par2", 16000},
	}
}

func releasedSet(j *Job, n int) []int {
	var out []int
	for i := 1; i <= n; i++ {
		if j.Progress().FileFetchPolicy(i) == FetchAlways {
			out = append(out, i)
		}
	}
	return out
}

func TestReleaseRecovery(t *testing.T) {
	all := []int{1, 2, 3, 4, 5}
	tests := []struct {
		name   string
		vols   []heldVol
		sizing RecoverySizing
		prep   func(*Job)
		want   []int // released file indices
	}{
		{"sized chooses the byte-minimal cover", standardVols(), NeedBlocks(9), nil, []int{1, 4}},
		{"insufficient candidates release all", standardVols(), NeedBlocks(32), nil, all},
		{"zero value releases all", standardVols(), RecoverySizing{}, nil, all},
		{"Unsized releases all", standardVols(), Unsized(), nil, all},
		{"NeedBlocks(0) releases nothing", standardVols(), NeedBlocks(0), nil, nil},
		{"negative need releases nothing", standardVols(), NeedBlocks(-4), nil, nil},
		{"hopeless job releases nothing", standardVols(), Unsized(), func(j *Job) {
			j.progress.files[0].FailedBytes = 1 << 40
		}, nil},
		{"evicted manifest releases all", standardVols(), NeedBlocks(1), func(j *Job) { j.Evict() }, all},
		{"a minus-form volume is not a candidate", []heldVol{{"x.vol03-07.par2", 1000}}, NeedBlocks(1), nil, []int{1}},
		{"minus-form beside counted volumes is skipped", []heldVol{
			{"x.vol00-01.par2", 1}, {"x.vol00+02.par2", 5000}, {"x.vol02+02.par2", 6000},
		}, NeedBlocks(3), nil, []int{2, 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j := releaseJob(t, tc.vols)
			if tc.prep != nil {
				tc.prep(j)
			}
			got, err := j.ReleaseRecovery(tc.sizing)
			if err != nil {
				t.Fatalf("ReleaseRecovery: %v", err)
			}
			set := releasedSet(j, len(tc.vols))
			if !slices.Equal(set, tc.want) {
				t.Errorf("released files %v, want %v", set, tc.want)
			}
			if got != len(tc.want) {
				t.Errorf("returned %d, want %d", got, len(tc.want))
			}
			for i := 1; i <= len(tc.vols); i++ {
				if !slices.Contains(tc.want, i) && j.Progress().FileFetchPolicy(i) != FetchIfNeeded {
					t.Errorf("file %d policy = %v, want FetchIfNeeded", i, j.Progress().FileFetchPolicy(i))
				}
			}
		})
	}
}

func TestRecoveryReleased(t *testing.T) {
	// File 0 is content, file 1 the par2 index (not a recovery volume), and
	// files 2..4 are three recovery volumes.
	build := func(t *testing.T) (*Job, *Manifest) {
		t.Helper()
		art := func(id string) []JobArticle { return []JobArticle{{ID: id, Bytes: 100, Number: 1}} }
		m := NewManifest([]JobFile{
			{Subject: "data.rar", Bytes: 100, Articles: art("<d@x>")},
			{Subject: "x.par2", Bytes: 100, Articles: art("<i@x>")},
			{Subject: "x.vol00+01.par2", Bytes: 100, IsPar2Recovery: true, Articles: art("<v0@x>")},
			{Subject: "x.vol01+02.par2", Bytes: 100, IsPar2Recovery: true, Articles: art("<v1@x>")},
			{Subject: "x.vol03+04.par2", Bytes: 100, IsPar2Recovery: true, Articles: art("<v2@x>")},
		})
		j := New("rr", "rr", Policy{})
		if err := j.AttachContent(m); err != nil {
			t.Fatalf("AttachContent: %v", err)
		}
		return j, m
	}
	hold := func(t *testing.T, j *Job, fis ...int) {
		t.Helper()
		for _, fi := range fis {
			if err := j.SetFileFetchPolicy(fi, FetchIfNeeded); err != nil {
				t.Fatalf("SetFileFetchPolicy(%d): %v", fi, err)
			}
		}
	}
	tests := []struct {
		name                 string
		prep                 func(*testing.T, *Job)
		fetched, held, total int
	}{
		{"on-demand off: all fetched from ingest, no reason", func(*testing.T, *Job) {}, 0, 0, 3},
		{"reason set, one of three released", func(t *testing.T, j *Job) {
			hold(t, j, 3, 4)
			j.SetPar2ReleaseReason("repair needed")
		}, 1, 2, 3},
		{"reason set, none released", func(t *testing.T, j *Job) {
			hold(t, j, 2, 3, 4)
			j.SetPar2ReleaseReason("repair needed")
		}, 0, 3, 3},
		{"no reason: a FetchAlways volume is not a release", func(t *testing.T, j *Job) {
			hold(t, j, 3, 4)
		}, 0, 2, 3},
		{"a discarded volume counts as not fetched", func(t *testing.T, j *Job) {
			hold(t, j, 3)
			if err := j.SetFileFetchPolicy(4, FetchNever); err != nil {
				t.Fatal(err)
			}
			j.SetPar2ReleaseReason("repair needed")
		}, 1, 2, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j, m := build(t)
			tc.prep(t, j)
			f, h, n := j.Progress().RecoveryReleased(m)
			if f != tc.fetched || h != tc.held || n != tc.total {
				t.Errorf("RecoveryReleased = (%d, %d, %d), want (%d, %d, %d)", f, h, n, tc.fetched, tc.held, tc.total)
			}
		})
	}
	t.Run("nil progress", func(t *testing.T) {
		var p *JobProgress
		if f, h, n := p.RecoveryReleased(nil); f != 0 || h != 0 || n != 0 {
			t.Errorf("nil RecoveryReleased = (%d, %d, %d), want zeros", f, h, n)
		}
	})
}

func TestReleaseRecovery_NotResident(t *testing.T) {
	j := New("unattached", "x", Policy{})
	if _, err := j.ReleaseRecovery(Unsized()); !errors.Is(err, ErrNotResident) {
		t.Errorf("err = %v, want ErrNotResident", err)
	}
}

func TestReleaseRecovery_SkipsAlreadyReleased(t *testing.T) {
	j := releaseJob(t, standardVols())
	if err := j.SetFileFetchPolicy(4, FetchAlways); err != nil {
		t.Fatal(err)
	}
	got, err := j.ReleaseRecovery(Unsized())
	if err != nil || got != 4 {
		t.Errorf("released %d, err %v, want 4", got, err)
	}
}
