package queue

import (
	"encoding/json"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// recoveryFixture is a job with all three file kinds: content, a par2 index,
// and two recovery volumes. The index is the point of the fixture — it is the
// file whose classification this change corrects.
func recoveryFixture(t *testing.T) *Manifest {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: `"movie.mkv" yEnc`, Bytes: 1000, Articles: []nzb.Article{{ID: "c@x", Bytes: 1000, Number: 1}}},
		{Subject: `"movie.par2" yEnc`, Bytes: 50, Articles: []nzb.Article{{ID: "i@x", Bytes: 50, Number: 1}}},
		{Subject: `"movie.vol000+01.par2" yEnc`, Bytes: 300, Articles: []nzb.Article{{ID: "v1@x", Bytes: 300, Number: 1}}},
		{Subject: `"movie.vol001+02.par2" yEnc`, Bytes: 400, Articles: []nzb.Article{{ID: "v2@x", Bytes: 400, Number: 1}}},
	}}
	job, err := NewJob(parsed, AddOptions{Filename: "t.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	return m
}

// TestManifestRecoveryFigures_ExcludeTheIndex pins the correction this change
// exists to make: the par2 index is always downloaded and carries no recovery
// blocks, so it is content, not recovery capacity.
//
// The superseded figures keyed on isPar2File, a name-based predicate matching
// any ".par2" subject, so they counted the index as repair capacity that does
// not exist. On this fixture that was 750 B and 3 files against a true 700 B
// and 2 volumes.
func TestManifestRecoveryFigures_ExcludeTheIndex(t *testing.T) {
	t.Parallel()
	m := recoveryFixture(t)

	if got, want := m.RecoveryBytes(), int64(700); got != want {
		t.Errorf("RecoveryBytes() = %d, want %d (the two volumes only; %d would mean the 50 B index is still counted)",
			got, want, want+50)
	}
	if got, want := m.RecoveryFiles(), 2; got != want {
		t.Errorf("RecoveryFiles() = %d, want %d (volumes only; %d would mean the index is still counted)",
			got, want, want+1)
	}

	// Content is everything else, index included. Asserted through the
	// subtraction rather than an accessor, because no caller needs one.
	if got, want := m.TotalBytes()-m.RecoveryBytes(), int64(1050); got != want {
		t.Errorf("content bytes = %d, want %d (1000 B of content plus the 50 B index)", got, want)
	}
}

// TestManifestRecoveryFigures_PartitionIsExhaustive pins the arithmetic the
// whole design rests on: content and recovery partition the file set, so they
// sum to the total with nothing double-counted and nothing dropped.
func TestManifestRecoveryFigures_PartitionIsExhaustive(t *testing.T) {
	t.Parallel()
	m := recoveryFixture(t)

	var content int64
	for fi := range m.NumFiles() {
		if !m.FileIsPar2Recovery(fi) {
			content += m.FileBytes(fi)
		}
	}
	if got := content + m.RecoveryBytes(); got != m.TotalBytes() {
		t.Errorf("content(%d) + recovery(%d) = %d, want TotalBytes() = %d",
			content, m.RecoveryBytes(), got, m.TotalBytes())
	}
}

// TestManifestRecoveryFigures_NoVolumes covers the shape whose verdict this
// change flips: a job with a par2 index but no recovery volumes has zero
// repair capacity, where the old name-based figures reported the index's
// bytes as though they could repair something.
func TestManifestRecoveryFigures_NoVolumes(t *testing.T) {
	t.Parallel()
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: `"movie.mkv" yEnc`, Bytes: 1000, Articles: []nzb.Article{{ID: "c@x", Bytes: 1000, Number: 1}}},
		{Subject: `"movie.par2" yEnc`, Bytes: 50, Articles: []nzb.Article{{ID: "i@x", Bytes: 50, Number: 1}}},
	}}
	job, err := NewJob(parsed, AddOptions{Filename: "t.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	if got := m.RecoveryBytes(); got != 0 {
		t.Errorf("RecoveryBytes() = %d, want 0 — an index alone repairs nothing", got)
	}
	if got := m.RecoveryFiles(); got != 0 {
		t.Errorf("RecoveryFiles() = %d, want 0 — an index is not a recovery volume", got)
	}
}

// TestManifestRecoveryFigures_SurviveRoundTrip pins that the figures are
// recomputed correctly on load.
//
// They are no longer persisted. They used to be, so that the staleness
// DiscardDeferredPar2 deliberately left when it rebuilt the manifest against
// a reduced file set would survive a save/load cycle. That rebuild is gone,
// so the reason is gone with it, and a derived figure cannot drift from the
// file list the way a stored one can. is_par2_recovery is persisted per file,
// which is everything the recomputation needs.
func TestManifestRecoveryFigures_SurviveRoundTrip(t *testing.T) {
	t.Parallel()
	m := recoveryFixture(t)

	blob, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got Manifest
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.RecoveryBytes() != m.RecoveryBytes() {
		t.Errorf("RecoveryBytes() = %d after round-trip, want %d", got.RecoveryBytes(), m.RecoveryBytes())
	}
	if got.RecoveryFiles() != m.RecoveryFiles() {
		t.Errorf("RecoveryFiles() = %d after round-trip, want %d", got.RecoveryFiles(), m.RecoveryFiles())
	}
	if got.TotalBytes() != m.TotalBytes() {
		t.Errorf("TotalBytes() = %d after round-trip, want %d", got.TotalBytes(), m.TotalBytes())
	}
}

// TestManifestRecoveryFigures_LegacyBlobIgnoresStoredPar2 pins what happens
// to a manifest written before the recovery figures stopped being persisted.
//
// Such a blob carries par2_bytes/par2_files keys that no longer have fields to
// land in. encoding/json drops them, and the figures are recomputed from the
// per-file is_par2_recovery flags the blob also carries. The recomputation
// does not reproduce the stored values and is not meant to: the stored ones
// counted the par2 index, which is the overstatement this change removes.
func TestManifestRecoveryFigures_LegacyBlobIgnoresStoredPar2(t *testing.T) {
	t.Parallel()

	// Hand-written in the older on-disk shape: 750/3 counted the index.
	const legacy = `{
	  "files": [
	    {"subject":"movie.mkv","bytes":1000,"articles":[{"id":"c@x","bytes":1000,"number":1}]},
	    {"subject":"movie.par2","bytes":50,"articles":[{"id":"i@x","bytes":50,"number":1}]},
	    {"subject":"movie.vol000+01.par2","bytes":300,"is_par2_recovery":true,"articles":[{"id":"v1@x","bytes":300,"number":1}]},
	    {"subject":"movie.vol001+02.par2","bytes":400,"is_par2_recovery":true,"articles":[{"id":"v2@x","bytes":400,"number":1}]}
	  ],
	  "total_bytes": 1750,
	  "par2_bytes": 750,
	  "par2_files": 3
	}`

	var m Manifest
	if err := json.Unmarshal([]byte(legacy), &m); err != nil {
		t.Fatalf("Unmarshal legacy manifest: %v", err)
	}

	if got := m.NumFiles(); got != 4 {
		t.Fatalf("NumFiles() = %d, want 4 — the legacy blob did not load", got)
	}
	if got := m.TotalBytes(); got != 1750 {
		t.Errorf("TotalBytes() = %d, want 1750", got)
	}
	if got := m.RecoveryBytes(); got != 700 {
		t.Errorf("RecoveryBytes() = %d, want 700 — recomputed from is_par2_recovery, "+
			"not read from the blob's par2_bytes of 750", got)
	}
	if got := m.RecoveryFiles(); got != 2 {
		t.Errorf("RecoveryFiles() = %d, want 2 — the blob's par2_files of 3 counted the index", got)
	}
}

// TestRecoveryFigures covers the shared helper directly. newManifest and
// UnmarshalJSON both call it so the two construction paths cannot disagree,
// which is the property worth testing at this level rather than only through
// the accessors.
func TestRecoveryFigures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		files     []manifestFile
		wantBytes int64
		wantCount int
	}{
		{name: "empty", files: nil},
		{
			name:  "content only",
			files: []manifestFile{{bytes: 100}, {bytes: 200}},
		},
		{
			name:      "volumes only",
			files:     []manifestFile{{bytes: 100, isPar2Recovery: true}, {bytes: 200, isPar2Recovery: true}},
			wantBytes: 300, wantCount: 2,
		},
		{
			name: "mixed, index not flagged",
			files: []manifestFile{
				{bytes: 1000},
				{bytes: 50}, // the par2 index: not flagged as recovery
				{bytes: 300, isPar2Recovery: true},
			},
			wantBytes: 300, wantCount: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotBytes, gotCount := recoveryFigures(tc.files)
			if gotBytes != tc.wantBytes || gotCount != tc.wantCount {
				t.Errorf("recoveryFigures() = (%d, %d), want (%d, %d)",
					gotBytes, gotCount, tc.wantBytes, tc.wantCount)
			}
		})
	}
}
