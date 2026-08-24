package durability

import (
	"strings"
	"testing"
)

// TestCollisionFindings_GroupsPerFile pins the collapse from collisions to
// anomalies.
//
// One anomaly per FILE, not per collision, because the barrier's admit latch is
// keyed on (job, file) and would discard the rest anyway — a per-collision
// return would build reports that are guaranteed to be thrown away, and hide
// that fact from anyone reading this function alone.
//
// FinalizeFile's own test covers a single collision on a single file end to
// end. It cannot reach this: a finalize is scoped to one file, so the grouping
// and the file ordering below are only exercised through Barrier.Run, where a
// two-file collision is not constructible without a fixture larger than the
// property being checked.
func TestCollisionFindings_GroupsPerFile(t *testing.T) {
	path := func(idx int32) string {
		return map[int32]string{0: "/d/first.rar", 1: "/d/second.rar"}[idx]
	}

	got := collisionFindings([]Collision{
		{FileIdx: 1, Offset: 0, Kept: 10, Dropped: 12},
		{FileIdx: 0, Offset: 0, Kept: 0, Dropped: 3},
		{FileIdx: 1, Offset: 500, Kept: 14, Dropped: 15},
	}, path)

	if len(got) != 2 {
		t.Fatalf("collisionFindings returned %d anomalies, want 2 — one per file, "+
			"and file 1's two collisions must fold into one: %+v", len(got), got)
	}

	// First-seen order, not sorted: the caller appends these ahead of the
	// overlap findings and the barrier latches per file, so a stable order is
	// what keeps a two-file report reproducible under test.
	if got[0].FileIdx != 1 || got[1].FileIdx != 0 {
		t.Errorf("anomaly file order = [%d %d], want [1 0] — files must appear in "+
			"the order their first collision did", got[0].FileIdx, got[1].FileIdx)
	}

	// File 1's report must carry BOTH of its collisions; folding to one
	// anomaly must not silently drop the second pair.
	for _, want := range []string{"second.rar", "10 and 12", "14 and 15", "offset 0", "offset 500"} {
		if !strings.Contains(got[0].Reason, want) {
			t.Errorf("file 1's reason = %q, want it to contain %q — the collapse is "+
				"per file, so every pair on that file has to survive into the text",
				got[0].Reason, want)
		}
	}
	if strings.Contains(got[1].Reason, "second.rar") {
		t.Errorf("file 0's reason = %q names file 1; each anomaly must resolve its "+
			"OWN path or a user acts on the wrong file", got[1].Reason)
	}
}

// TestCollisionFindings_EmptyReportsNothing pins that the ordinary commit —
// which drops nothing — costs no allocation and, more importantly, raises no
// warning. Every checkpoint of every healthy job takes this path.
func TestCollisionFindings_EmptyReportsNothing(t *testing.T) {
	called := false
	got := collisionFindings(nil, func(int32) string {
		called = true
		return "/d/never.rar"
	})
	if got != nil {
		t.Errorf("collisionFindings(nil) = %+v, want nil", got)
	}
	if called {
		t.Error("the path resolver ran with no collisions to report; it takes the " +
			"pipeline's RLock, so the empty case must not reach it")
	}
}

// TestCollisionReason_SaysRepairableNotCorrupt pins the honesty of the message,
// which is a real constraint rather than a style note.
//
// Two articles claiming one offset has two outcomes and this layer cannot
// distinguish them: resolved inside a single open-file episode the file
// completes SHORT and is benign; unresolved across a close-handles cycle or a
// restart the later write overwrites the earlier and the file completes WRONG.
// Only the second is corruption. Asserting corruption would be wrong on the
// commoner case — a multi-part UU post — and asserting nothing would leave the
// user with an unexplained repair.
func TestCollisionReason_SaysRepairableNotCorrupt(t *testing.T) {
	got := collisionReason("/downloads/job/data.rar", []Collision{
		{FileIdx: 0, Offset: 0, Kept: 4, Dropped: 7},
	})

	// The base name, so the user can find the file; not the directory, which
	// is noise in a job-level warning.
	if !strings.Contains(got, "data.rar") || strings.Contains(got, "/downloads") {
		t.Errorf("reason = %q, want the base name and not the full path", got)
	}
	if !strings.Contains(got, "par2") {
		t.Errorf("reason = %q, want it to name par2 as the remedy — the user's "+
			"question is why a healthy-looking job needed repairing", got)
	}
	for _, forbidden := range []string{"corrupt", "failed", "lost"} {
		if strings.Contains(strings.ToLower(got), forbidden) {
			t.Errorf("reason = %q contains %q; this layer cannot tell a short file "+
				"from an overwritten one, and the in-episode case — an ordinary "+
				"multi-part UU post — is neither corrupt nor failed", got, forbidden)
		}
	}
}
