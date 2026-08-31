package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/nzb"
)

// The parser's anomaly counters had no reader before this: an NZB could lose
// segments to a size check or a repeated Message-ID and say so to nobody, and
// the user's first evidence would be a file that never completes. The warning
// has to name what was dropped, not merely that something was.
func TestLogParseAnomalies_ReportsWhatWasDiscarded(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	logParseAnomalies(logger, "broken.nzb", &nzb.NZB{
		DuplicateMessageIDs: 2,
		DuplicateArticles:   1,
		BadArticles:         3,
		SkippedFiles:        1,
	})

	out := buf.String()
	for _, want := range []string{
		"broken.nzb",
		"duplicate_message_ids=2",
		"duplicate_part_numbers=1",
		"implausible_size=3",
		"files_without_usable_segments=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("warning does not report %q\ngot: %s", want, out)
		}
	}
}

// A clean NZB must not produce a warning, or the signal is worthless.
func TestLogParseAnomalies_SilentOnACleanNZB(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	logParseAnomalies(logger, "fine.nzb", &nzb.NZB{})

	if buf.Len() != 0 {
		t.Errorf("clean NZB produced a warning: %s", buf.String())
	}
}

// Two call sites pass a logger that can be nil, and one passes a parsed
// document that a failed parse leaves nil. Neither may panic during ingest.
func TestLogParseAnomalies_ToleratesNilInputs(t *testing.T) {
	t.Parallel()
	logParseAnomalies(nil, "x.nzb", &nzb.NZB{BadArticles: 1})

	var buf bytes.Buffer
	logParseAnomalies(slog.New(slog.NewTextHandler(&buf, nil)), "x.nzb", nil)
	if buf.Len() != 0 {
		t.Errorf("nil NZB produced output: %s", buf.String())
	}
}
