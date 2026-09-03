package app

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

// The queue's per-file writers report a removed or evicted job as
// ErrNotFound/ErrNotResident specifically so a caller can tell that case
// from a genuine write failure (#261). That distinction is only worth
// anything if callers act on it: routine mid-flight eviction must not read as
// a warning, and a real failure must not be demoted to Debug because the
// non-resident case is common.
func TestLogQueueWriteFailure_LevelsBySeverity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantLevel string
	}{
		{"not found", fmt.Errorf("%w: j1", dispatch.ErrNotFound), "DEBUG"},
		{"not resident", fmt.Errorf("%w: j1", job.ErrNotResident), "DEBUG"},
		{"wrapped not resident", fmt.Errorf("set crc: %w", fmt.Errorf("%w: j1", job.ErrNotResident)), "DEBUG"},
		{"index out of range", errors.New("queue: fileIdx 9 out of range for job j1"), "WARN"},
		{"store failure", errors.New("sqlite: disk I/O error"), "WARN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			app := &Application{log: slog.New(slog.NewTextHandler(&buf,
				&slog.HandlerOptions{Level: slog.LevelDebug}))}

			app.logQueueWriteFailure("set file CRC32", "j1", 3, tc.err)

			logged := buf.String()
			if !strings.Contains(logged, "level="+tc.wantLevel) {
				t.Errorf("logged at the wrong level for %v\n  want level=%s\n  got:  %s",
					tc.err, tc.wantLevel, strings.TrimSpace(logged))
			}
			// The operation and the file index have to survive into the
			// record, or the line cannot be acted on.
			if !strings.Contains(logged, "set file CRC32") || !strings.Contains(logged, "fileidx=3") {
				t.Errorf("log line dropped the operation or file index: %s", strings.TrimSpace(logged))
			}
		})
	}
}
