package job

import (
	"testing"
	"time"
)

func TestDownloadSeconds(t *testing.T) {
	t.Parallel()
	start := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		name string
		p    *JobProgress
		want int64
	}{
		{"nil progress", nil, 1},
		{"no stamps", &JobProgress{}, 1},
		{"start only", &JobProgress{downloadStarted: start}, 1},
		{"under a second", &JobProgress{downloadStarted: start, downloadFinished: start.Add(400 * time.Millisecond)}, 1},
		{"finish before start", &JobProgress{downloadStarted: start, downloadFinished: start.Add(-5 * time.Second)}, 1},
		{"whole seconds", &JobProgress{downloadStarted: start, downloadFinished: start.Add(35 * time.Second)}, 35},
	} {
		if got := tc.p.DownloadSeconds(); got != tc.want {
			t.Errorf("%s: DownloadSeconds() = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestWrittenBytes(t *testing.T) {
	t.Parallel()
	if got := WrittenBytes(1000, 100, 200); got != 700 {
		t.Errorf("WrittenBytes(1000,100,200) = %d, want 700", got)
	}
	if got := WrittenBytes(100, 200, 0); got != 0 {
		t.Errorf("WrittenBytes of a torn read = %d, want 0", got)
	}
}
