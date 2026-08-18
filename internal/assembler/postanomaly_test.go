package assembler

import (
	"bytes"
	"strings"
	"testing"
)

// TestPostAnomaly_ReportedOncePerFileAlongsideEachRejection pins the pairing
// #379 asks for: every displaced article is still resolved individually, and
// the user is told ONCE what shape of fault produced them.
//
// The per-article report and the per-job one answer different questions. The
// rejection is accounting — it charges the article's bytes against the par2
// budget. The anomaly is diagnosis, and without it a malformed post is
// indistinguishable to a user from a disk or network problem, which is the
// whole of the issue.
func TestPostAnomaly_ReportedOncePerFileAlongsideEachRejection(t *testing.T) {
	a := newHelperAssembler()
	var rejected []int32
	var anomalies []string
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		rejected = append(rejected, artIdx)
	}
	a.opts.OnPostAnomaly = func(_ string, _ int, reason string) {
		anomalies = append(anomalies, reason)
	}

	// Two collisions on one file, as an obfuscated post colliding on several
	// segments produces. Only the first carries firstCollision.
	a.routeFaulted([]faultedArticle{
		{
			id: articleID{msgID: "a", artIdx: 1}, displaced: true,
			offset: 0, displacedBy: articleID{msgID: "b", artIdx: 2},
			firstCollision: true,
		},
		{
			id: articleID{msgID: "b", artIdx: 2}, displaced: true,
			offset: 0, displacedBy: articleID{msgID: "c", artIdx: 3},
		},
	}, "job", 0, "/downloads/movie.part01.rar")

	if len(rejected) != 2 {
		t.Errorf("rejected = %v, want both articles — the per-article accounting is "+
			"unchanged by the job-level report", rejected)
	}
	if len(anomalies) != 1 {
		t.Fatalf("OnPostAnomaly fired %d times, want 1: job.Warning is single-valued, "+
			"so a post colliding on every segment would overwrite it repeatedly",
			len(anomalies))
	}

	got := anomalies[0]
	for _, want := range []string{"movie.part01.rar", "<a>", "<b>", "offset 0"} {
		if !strings.Contains(got, want) {
			t.Errorf("anomaly reason %q does not name %q — the user cannot check the "+
				"claim against the NZB without it", got, want)
		}
	}
}

// TestPostAnomaly_ReasonDoesNotDiagnoseTheCause guards the wording, which is
// the deliberate part of this feature rather than incidental phrasing.
//
// Three different faults produce an identical observation: a malformed post, a
// redundant posting whose copies are both valid, and a server mangling the
// =ypart begin= digits in transit. Nothing in this program can separate them —
// yEnc checksums the decoded payload and never its headers — and with no
// failover there is no cross-server evidence either. So the message reports
// what was seen and stops.
func TestPostAnomaly_ReasonDoesNotDiagnoseTheCause(t *testing.T) {
	got := postAnomalyReason("/downloads/movie.part01.rar", faultedArticle{
		id: articleID{msgID: "first", artIdx: 1}, displaced: true,
		offset: 4096, displacedBy: articleID{msgID: "second", artIdx: 2},
	})

	for _, verdict := range []string{"malformed", "corrupt", "bad post", "invalid"} {
		if strings.Contains(strings.ToLower(got), verdict) {
			t.Errorf("reason %q asserts %q, which this package cannot know: a redundant "+
				"posting and a server-side mangling of the offset digits produce the "+
				"same observation", got, verdict)
		}
	}
	// WarningsBanner.svelte counts duplicates by SUBSTRING, so a reason that
	// merely contains the phrase inflates that count. Equality was the right
	// test while the banner compared exactly; it is now too weak to catch the
	// collision it exists for.
	if strings.Contains(got, "Duplicate NZB") {
		t.Error("the reason contains 'Duplicate NZB', which WarningsBanner would " +
			"tally as a duplicate job")
	}
}

// TestPostAnomaly_ReasonIdentifiesAnArticleWithNoMessageID covers the fallback.
// An NZB may omit the Message-ID, and printing an empty <> where an identifier
// belongs would make the warning unusable on exactly the malformed posts this
// feature exists to describe.
func TestPostAnomaly_ReasonIdentifiesAnArticleWithNoMessageID(t *testing.T) {
	got := postAnomalyReason("/downloads/x.rar", faultedArticle{
		id: articleID{msgID: "", artIdx: 7}, displaced: true,
		offset: 512, displacedBy: articleID{msgID: "", artIdx: 8},
	})

	if !strings.Contains(got, "#7") || !strings.Contains(got, "#8") {
		t.Errorf("reason %q does not identify articles that carry no Message-ID", got)
	}
	if strings.Contains(got, "<>") {
		t.Errorf("reason %q prints an empty Message-ID where an identifier belongs", got)
	}
}

// TestArticleLabel covers the identifier the warning is built from, including
// the fallback that only malformed NZBs reach.
func TestArticleLabel(t *testing.T) {
	tests := []struct {
		name string
		id   articleID
		want string
	}{
		{
			name: "a Message-ID is bracketed as it appears in the NZB",
			id:   articleID{msgID: "abc@news", artIdx: 3},
			want: "<abc@news>",
		},
		{
			name: "an article with none falls back to its index",
			id:   articleID{msgID: "", artIdx: 3},
			want: "#3",
		},
		{
			name: "index zero is a real article, not a missing one",
			id:   articleID{msgID: "", artIdx: 0},
			want: "#0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := articleLabel(tc.id); got != tc.want {
				t.Errorf("articleLabel(%+v) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

// TestFileWriter_FirstCollisionMarksOnlyTheFirst pins the latch at its source.
// routeFaulted trusts the flag, so a writer that set it on every record would
// reinstate the repeated-overwrite behaviour with no test above noticing.
func TestFileWriter_FirstCollisionMarksOnlyTheFirst(t *testing.T) {
	// A cache large enough that no contiguous run forms, so each incumbent is
	// still buffered and unwritten when the next arrives — the only shape that
	// reaches failDisplaced at all. A written incumbent settles its offset and
	// acceptArticle refuses the arrival instead.
	w := newTestFileWriter(t, withCacheBytes(1<<20))

	for _, id := range []articleID{
		{msgID: "a", artIdx: 1}, {msgID: "b", artIdx: 2}, {msgID: "c", artIdx: 3},
	} {
		w.admitAccepted(id.msgID)
		if err := w.Accept(id, 0, append([]byte(nil), bytes.Repeat([]byte{'x'}, 64)...)); err != nil {
			t.Fatalf("accept %s: %v", id.msgID, err)
		}
	}

	rolled := w.takeFaulted()
	if len(rolled) != 2 {
		t.Fatalf("got %d displaced articles, want 2", len(rolled))
	}
	if !rolled[0].firstCollision {
		t.Error("the first collision on the file is not marked, so no warning is raised at all")
	}
	if rolled[1].firstCollision {
		t.Error("a second collision on the same file is marked first, which is what " +
			"overwrites job.Warning once per colliding segment")
	}

	// The detail the warning is built from has to survive the trip.
	if rolled[0].offset != 0 || rolled[0].displacedBy != (articleID{msgID: "b", artIdx: 2}) {
		t.Errorf("displaced record = %+v, want offset 0 displaced by b — a record missing "+
			"its displacer cannot describe the collision", rolled[0])
	}
}
