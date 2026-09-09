package job

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// TestRunsCoverage covers the bounds checks that decide whether a durability
// Run names articles the manifest actually has. Both rejections matter: a run
// naming a file that does not exist, and a run whose article span escapes the
// file it claims.
func TestRunsCoverage(t *testing.T) {
	m := NewManifest([]JobFile{
		{Subject: "f0", Bytes: 200, Articles: []JobArticle{
			{ID: "<a0@x>", Bytes: 100, Number: 1},
			{ID: "<a1@x>", Bytes: 100, Number: 2},
		}},
		{Subject: "f1", Bytes: 100, Articles: []JobArticle{
			{ID: "<b0@x>", Bytes: 100, Number: 1},
		}},
	})

	tests := []struct {
		name      string
		run       durability.Run
		wantFirst int
		wantLast  int
		wantErr   bool
	}{
		{name: "whole first file", run: durability.Run{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 1}, wantFirst: 0, wantLast: 1},
		{name: "single article of second file", run: durability.Run{FileIdx: 1, FirstArtIdx: 2, LastArtIdx: 2}, wantFirst: 2, wantLast: 2},
		{name: "file index past the end", run: durability.Run{FileIdx: 2, FirstArtIdx: 0, LastArtIdx: 0}, wantErr: true},
		{name: "negative file index", run: durability.Run{FileIdx: -1, FirstArtIdx: 0, LastArtIdx: 0}, wantErr: true},
		{name: "inverted span", run: durability.Run{FileIdx: 0, FirstArtIdx: 1, LastArtIdx: 0}, wantErr: true},
		{name: "span starts before the file", run: durability.Run{FileIdx: 1, FirstArtIdx: 1, LastArtIdx: 2}, wantErr: true},
		{name: "span runs past the file", run: durability.Run{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 2}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first, last, err := runsCoverage(m, tc.run)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("runsCoverage(%+v) = %d, %d, nil; want an error", tc.run, first, last)
				}
				return
			}
			if err != nil {
				t.Fatalf("runsCoverage(%+v): %v", tc.run, err)
			}
			if first != tc.wantFirst || last != tc.wantLast {
				t.Errorf("runsCoverage(%+v) = %d, %d; want %d, %d", tc.run, first, last, tc.wantFirst, tc.wantLast)
			}
		})
	}
}

// TestUndeferRecovery covers the sole writer of par2Recovered. Its return value
// is load-bearing: MarkArticleFailed records a release reason only when this
// reports a change, so a wrong "true" would attach a reason to a job whose
// volumes were never released.
func TestUndeferRecovery(t *testing.T) {
	newHeld := func(t *testing.T) *Job {
		t.Helper()
		m := NewManifest([]JobFile{
			{Subject: "data", Bytes: 100, Articles: []JobArticle{{ID: "<d0@x>", Bytes: 100, Number: 1}}},
			{Subject: "vol01+02.par2", Bytes: 100, IsPar2Recovery: true,
				Articles: []JobArticle{{ID: "<p0@x>", Bytes: 100, Number: 1}}},
		})
		j := New("j", "j", Policy{})
		if err := j.AttachContent(m); err != nil {
			t.Fatalf("AttachContent: %v", err)
		}
		j.progress.files[1].Fetch = FetchIfNeeded
		return j
	}

	t.Run("releases a held volume and records the flag", func(t *testing.T) {
		j := newHeld(t)
		if j.progress.Par2Recovered() {
			t.Fatal("fixture guard: Par2Recovered must start false")
		}
		if !j.undeferRecovery([]int{1}) {
			t.Fatal("undeferRecovery reported no change for a held volume")
		}
		if j.progress.files[1].Fetch != FetchAlways {
			t.Errorf("Fetch = %v, want FetchAlways", j.progress.files[1].Fetch)
		}
		if !j.progress.Par2Recovered() {
			t.Error("Par2Recovered = false after a release, want true")
		}
	})

	t.Run("reports no change when nothing was held", func(t *testing.T) {
		j := newHeld(t)
		// File 0 is ordinary data and already FetchAlways, so there is nothing
		// to release and the flag must stay false.
		if j.undeferRecovery([]int{0}) {
			t.Error("undeferRecovery reported a change for an already-FetchAlways file")
		}
		if j.progress.Par2Recovered() {
			t.Error("Par2Recovered = true without any release")
		}
	})

	t.Run("ignores out-of-range indices without reporting a change", func(t *testing.T) {
		j := newHeld(t)
		if j.undeferRecovery([]int{-1, 99}) {
			t.Error("undeferRecovery reported a change for out-of-range indices")
		}
		if j.progress.Par2Recovered() {
			t.Error("Par2Recovered = true after only out-of-range indices")
		}
	})
}
