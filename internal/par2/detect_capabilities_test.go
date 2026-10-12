package par2

import (
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/testutil"
)

// TestDetectCapabilities parses fake par2 binaries' -h and -V output, and
// returns zero caps for a binary that cannot run.
func TestDetectCapabilities(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		help   string
		vers   string
		want   Caps
		noExec bool
	}{
		{
			name: "turbo with -N and -B, help exiting non-zero",
			help: "Usage:\n  -N  : No data skipping\n  -B<path> : Set the basepath\n  -t+ : threads",
			vers: "par2cmdline-turbo version 1.1.0\nCopyright",
			want: Caps{SupportsNoDataSkipping: true, SupportsBasepath: true, IsTurbo: true, Version: "1.1.0"},
		},
		{
			name: "plain build with a bare version line",
			help: "Usage: par2 c|v|r",
			vers: "0.8.1",
			want: Caps{Version: "0.8.1"},
		},
		{
			name:   "missing binary",
			noExec: true,
			want:   Caps{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin := filepath.Join(t.TempDir(), "par2")
			if !tc.noExec {
				testutil.WriteExecutable(t, bin, "#!/bin/sh\n"+
					"if [ \"$1\" = -V ]; then printf '%s\\n' '"+tc.vers+"'; exit 0; fi\n"+
					"printf '%s\\n' '"+tc.help+"'\nexit 1\n")
			}
			if got := DetectCapabilities(t.Context(), bin); got != tc.want {
				t.Errorf("DetectCapabilities = %+v, want %+v", got, tc.want)
			}
		})
	}
}
