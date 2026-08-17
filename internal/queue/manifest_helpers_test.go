package queue

import "testing"

// Pre-existing helpers in manifest.go with no direct test. They are covered
// indirectly through the exported API, which is why the gap survived; these
// exercise them head-on so a change to either is caught at its own level
// rather than through whatever happened to call it.

// isPar2File matches on the ".par2" substring anywhere in the subject, which
// is deliberately broader than isPar2Recovery's volume-name convention: this
// one also matches the index file. The two are not interchangeable, and
// counting an index as repair capacity is the bug that distinction exists to
// prevent — so the index case is pinned as a MATCH here on purpose.
func TestIsPar2File(t *testing.T) {
	for _, tc := range []struct {
		subject string
		want    bool
	}{
		{"archive.vol000+01.par2", true},
		{"archive.par2", true},
		{"ARCHIVE.PAR2", true},
		{"Archive.Par2", true},
		{"archive.par2.bin", true},
		{"archive.rar", false},
		{"archive.par3", false},
		{"par2", false},
		{"", false},
	} {
		if got := isPar2File(tc.subject); got != tc.want {
			t.Errorf("isPar2File(%q) = %v, want %v", tc.subject, got, tc.want)
		}
	}
}

// buildMessageIDIndex is last-writer-wins over articleIDs. That only stays
// harmless because internal/nzb refuses a repeated Message-ID at parse time,
// so the index is injective for any manifest the parser produced — this test
// records the collision behaviour so that if the parse-time rule is ever
// relaxed, the consequence here is already written down.
func TestBuildMessageIDIndex(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.bin", Articles: []JobArticle{
			{ID: "a@h", Bytes: 1, Number: 1},
			{ID: "b@h", Bytes: 1, Number: 2},
		}},
		{Subject: "b.bin", Articles: []JobArticle{
			{ID: "c@h", Bytes: 1, Number: 1},
		}},
	})

	m.buildMessageIDIndex()
	if got := len(m.messageIDIndex); got != 3 {
		t.Fatalf("len(messageIDIndex) = %d, want 3", got)
	}
	for id, want := range map[string]int{"a@h": 0, "b@h": 1, "c@h": 2} {
		if got, ok := m.messageIDIndex[id]; !ok || got != want {
			t.Errorf("messageIDIndex[%q] = %d (present %v), want %d", id, got, ok, want)
		}
	}

	// The index spans the whole job, not one file: "c@h" lives in the
	// second file and still resolves to its job-wide article index.
	if got := m.messageIDIndex["c@h"]; got != 2 {
		t.Errorf("cross-file lookup = %d, want 2", got)
	}

	// Rebuilding is idempotent — it replaces the map rather than merging
	// into a stale one.
	m.buildMessageIDIndex()
	if got := len(m.messageIDIndex); got != 3 {
		t.Errorf("len after rebuild = %d, want 3", got)
	}
}

// A duplicate ID resolves to the LAST index carrying it. No manifest built
// from a parsed NZB can contain one, so this documents the failure mode
// rather than a supported case.
func TestBuildMessageIDIndexDuplicateResolvesToLast(t *testing.T) {
	m := newManifest([]JobFile{{Subject: "a.bin", Articles: []JobArticle{
		{ID: "dup@h", Bytes: 1, Number: 1},
		{ID: "dup@h", Bytes: 1, Number: 2},
	}}})

	m.buildMessageIDIndex()
	if got := len(m.messageIDIndex); got != 1 {
		t.Fatalf("len(messageIDIndex) = %d, want 1 — the two entries collide", got)
	}
	if got := m.messageIDIndex["dup@h"]; got != 1 {
		t.Errorf("messageIDIndex[dup@h] = %d, want 1 (last writer wins)", got)
	}
}
