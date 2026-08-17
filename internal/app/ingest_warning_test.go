package app

import (
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/nzb"
)

// The log line reaches an operator tailing the daemon; job.Warning reaches the
// person looking at the queue, which is the audience that has to decide whether
// a short download is worth re-adding from another indexer. A parse anomaly
// that only ever appears in a log is invisible to them.
func TestParseAnomalySummary_NamesEachDiscardKind(t *testing.T) {
	got := parseAnomalySummary(&nzb.NZB{
		DuplicateMessageIDs: 2,
		DuplicateArticles:   1,
		BadArticles:         3,
		SkippedFiles:        1,
	})

	for _, want := range []string{
		"2 repeated message-id",
		"1 duplicate part number",
		"3 implausible size",
		"1 file with no usable segments",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary does not report %q\ngot: %s", want, got)
		}
	}
}

// A clean document must produce no warning, or every job carries one and the
// queue's warning column stops meaning anything.
func TestParseAnomalySummary_EmptyForACleanNZB(t *testing.T) {
	if got := parseAnomalySummary(&nzb.NZB{}); got != "" {
		t.Errorf("clean NZB produced a warning: %q", got)
	}
	if got := parseAnomalySummary(nil); got != "" {
		t.Errorf("nil NZB produced a warning: %q", got)
	}
}

// Only the counters that fired are named, so the warning stays readable in a
// queue row rather than listing four zeroes.
func TestParseAnomalySummary_OmitsCountersThatDidNotFire(t *testing.T) {
	got := parseAnomalySummary(&nzb.NZB{DuplicateMessageIDs: 1})

	if !strings.Contains(got, "1 repeated message-id") {
		t.Errorf("summary does not report the counter that fired: %s", got)
	}
	for _, unwanted := range []string{"duplicate part number", "implausible size", "no usable segments"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("summary names %q though its counter was zero: %s", unwanted, got)
		}
	}
}
