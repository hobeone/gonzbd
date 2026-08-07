package queue

import "testing"

// TestContentFailedBytes_ExcludesPar2 covers the accessor directly: only
// non-par2 files contribute, and the par2 index counts as par2 even though it
// is not a recovery volume.
func TestContentFailedBytes_ExcludesPar2(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files []FileMeta
		want  int64
	}{
		{name: "no files"},
		{
			name:  "content only",
			files: []FileMeta{{ArticleCount: 1, FailedBytes: 100}, {ArticleCount: 1, FailedBytes: 50}},
			want:  150,
		},
		{
			name: "a failed recovery volume is not damage",
			files: []FileMeta{
				{ArticleCount: 1, FailedBytes: 100},
				{ArticleCount: 1, FailedBytes: 300, IsPar2: true},
			},
			want: 100,
		},
		{
			name: "a failed index is not damage either",
			files: []FileMeta{
				{ArticleCount: 1},
				{ArticleCount: 1, FailedBytes: 50, IsPar2: true},
			},
			want: 0,
		},
		{
			name: "everything par2 failed, content intact",
			files: []FileMeta{
				{ArticleCount: 1, FailedBytes: 50, IsPar2: true},
				{ArticleCount: 1, FailedBytes: 300, IsPar2: true},
			},
			want: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newJobProgressSized(tc.files)
			if got := p.ContentFailedBytes(); got != tc.want {
				t.Errorf("ContentFailedBytes() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestContentFailedBytes_NilReceiver matches the permissive convention of the
// accessors around it: a nil progress reports zero rather than panicking, so a
// reporting path cannot be brought down by a job in an unexpected state.
func TestContentFailedBytes_NilReceiver(t *testing.T) {
	t.Parallel()
	var p *JobProgress
	if got := p.ContentFailedBytes(); got != 0 {
		t.Errorf("ContentFailedBytes() on nil = %d, want 0", got)
	}
}

// TestContentFailedBytes_AgreesAcrossResidency is the property that makes this
// figure usable in an abort decision.
//
// Two abort gates read it, and one of them runs over Queue.Snapshot() during
// startup recovery, where a job may have no resident manifest. If the figure
// depended on residency, the same job would be condemned or spared according
// to whether it happened to be promoted — the exact class of drift this
// package has repeatedly had to close.
//
// It holds because the par2 classification is not stored in a column but
// derived from the subject, which both paths have: the manifest when resident,
// job_files.subject when not.
func TestContentFailedBytes_AgreesAcrossResidency(t *testing.T) {
	t.Parallel()

	// Resident: built from a manifest, classification from FileSubject.
	m := recoveryFixture(t)
	resident := newJobProgress(m)
	resident.files[0].FailedBytes = 120 // content
	resident.files[1].FailedBytes = 50  // the par2 index
	resident.files[2].FailedBytes = 300 // a recovery volume

	// Non-resident: built from FileMeta as SQLiteStore.ArticleCountsByJob
	// produces it, classification from the stored subject.
	meta := make([]FileMeta, m.NumFiles())
	for fi := range meta {
		lo, hi := m.FileRange(fi)
		meta[fi] = FileMeta{
			ArticleCount: hi - lo,
			Bytes:        m.FileBytes(fi),
			IsPar2:       isPar2File(m.FileSubject(fi)),
		}
	}
	meta[0].FailedBytes = 120
	meta[1].FailedBytes = 50
	meta[2].FailedBytes = 300
	nonResident := newJobProgressSized(meta)

	if got, want := resident.ContentFailedBytes(), int64(120); got != want {
		t.Errorf("resident ContentFailedBytes() = %d, want %d (content only; %d would count the par2 files)",
			got, want, want+350)
	}
	if resident.ContentFailedBytes() != nonResident.ContentFailedBytes() {
		t.Errorf("residency drift: resident = %d, non-resident = %d — the same job would be "+
			"condemned or spared depending on whether it happened to be promoted",
			resident.ContentFailedBytes(), nonResident.ContentFailedBytes())
	}
}
