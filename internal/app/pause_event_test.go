package app

import "testing"

// TestPauseResumeDownloads_BroadcastQueueUpdated pins that a global pause or
// resume pushes a queue update. The metrics tick only emits one while speed is
// above zero, and a pause drops speed to zero, so without this broadcast
// nothing tells a connected UI that the paused flag it last polled is stale.
func TestPauseResumeDownloads_BroadcastQueueUpdated(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		call func(*Application)
	}{
		{"pause", (*Application).PauseDownloads},
		{"resume", (*Application).ResumeDownloads},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			application := newTestApplication(t)
			rec := &recordingEmitter{}
			application.emitter = rec

			tc.call(application)

			var n int
			for _, e := range rec.events {
				if e.Type == "queue_updated" {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("%s broadcast %d queue_updated events (%+v), want 1", tc.name, n, rec.events)
			}
		})
	}
}
