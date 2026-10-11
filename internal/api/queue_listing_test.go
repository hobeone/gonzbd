package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/humanfmt"
)

// queueLevel is the queue-level part of a mode=queue listing that these tests
// assert on.
type queueLevel struct {
	Status         string `json:"status"`
	Paused         bool   `json:"paused"`
	Speed          string `json:"speed"`
	KBPerSec       string `json:"kbpersec"`
	MB             string `json:"mb"`
	MBLeft         string `json:"mbleft"`
	Size           string `json:"size"`
	SizeLeft       string `json:"sizeleft"`
	Timeleft       string `json:"timeleft"`
	NoOfSlots      int    `json:"noofslots"`
	NoOfSlotsTotal int    `json:"noofslots_total"`
}

func getQueueLevel(t *testing.T, s *Server, query string) queueLevel {
	t.Helper()
	rr := apiGet(t, s.Handler(), "/api?mode=queue"+query+"&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		Queue queueLevel `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Queue
}

// TestQueueList_SizeTotalsIgnorePagination pins #749: the queue-level size
// totals describe every slot the filters select, not the page start/limit
// cut out of them.
func TestQueueList_SizeTotalsIgnorePagination(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addNamedLargeTestJob(t, q, "ten", 10)
	addNamedLargeTestJob(t, q, "three", 3)

	const wantBytes = int64(13 << 20)
	got := getQueueLevel(t, s, "&limit=1")
	if got.NoOfSlots != 1 || got.NoOfSlotsTotal != 2 {
		t.Fatalf("noofslots = %d, noofslots_total = %d; want 1 and 2", got.NoOfSlots, got.NoOfSlotsTotal)
	}
	if want := toMBString(wantBytes); got.MB != want || got.MBLeft != want {
		t.Errorf("mb = %q, mbleft = %q; want %q for both (both jobs, not the page)", got.MB, got.MBLeft, want)
	}
	if want := humanfmt.Bytes(wantBytes); got.Size != want || got.SizeLeft != want {
		t.Errorf("size = %q, sizeleft = %q; want %q for both (both jobs, not the page)", got.Size, got.SizeLeft, want)
	}

	// start=1 selects the other job; the totals must not move.
	if got2 := getQueueLevel(t, s, "&start=1&limit=1"); got2.MB != got.MB || got2.Size != got.Size {
		t.Errorf("start=1: mb = %q, size = %q; want %q and %q", got2.MB, got2.Size, got.MB, got.Size)
	}
}

// TestQueueList_SpeedFields pins #734: the queue-level speed, kbpersec and
// timeleft come from the request's speed snapshot, not literals.
func TestQueueList_SpeedFields(t *testing.T) {
	t.Parallel()
	const speed = 2 * 1024 * 1024 // 2 MiB/s
	s, q := testQueueServerWithSpeed(t, speed)
	tj := addNamedLargeTestJob(t, q, "ten", 10) // 10 MiB → 5 s at 2 MiB/s
	if err := q.SetStatusIf(tj.ID, constants.StatusDownloading, constants.StatusQueued); err != nil {
		t.Fatalf("SetStatusIf: %v", err)
	}

	got := getQueueLevel(t, s, "")
	if got.Speed != "2.0 MB/s" {
		t.Errorf("speed = %q; want %q", got.Speed, "2.0 MB/s")
	}
	if got.KBPerSec != "2048.0" {
		t.Errorf("kbpersec = %q; want %q", got.KBPerSec, "2048.0")
	}
	if got.Timeleft != "0:00:05" {
		t.Errorf("timeleft = %q; want %q", got.Timeleft, "0:00:05")
	}

	// The single-job detail variant reports the same three fields.
	detail := getQueueLevel(t, s, "&nzo_id="+tj.ID+"&files=1")
	if detail.Speed != got.Speed || detail.KBPerSec != got.KBPerSec || detail.Timeleft != got.Timeleft {
		t.Errorf("detail speed/kbpersec/timeleft = %q/%q/%q; want %q/%q/%q",
			detail.Speed, detail.KBPerSec, detail.Timeleft, got.Speed, got.KBPerSec, got.Timeleft)
	}
}

// TestQueueList_TimeleftZeroWhenPaused pins the paused half of #734: a
// globally paused queue reports no ETA, whatever the speed snapshot says.
func TestQueueList_TimeleftZeroWhenPaused(t *testing.T) {
	t.Parallel()
	s, q := testQueueServerWithSpeed(t, 2*1024*1024)
	tj := addNamedLargeTestJob(t, q, "ten", 10)
	if err := q.SetStatusIf(tj.ID, constants.StatusDownloading, constants.StatusQueued); err != nil {
		t.Fatalf("SetStatusIf: %v", err)
	}
	q.PauseAll()

	if got := getQueueLevel(t, s, ""); got.Timeleft != "0:00:00" {
		t.Errorf("timeleft = %q; want 0:00:00 while paused", got.Timeleft)
	}
}

func TestFormatSpeed(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		bps  float64
		want string
	}{
		{0, "0 B/s"},
		{512, "512 B/s"},
		{1023.7, "1024 B/s"},
		{1024, "1.0 KB/s"},
		{1536, "1.5 KB/s"},
		{1024 * 1024, "1.0 MB/s"},
		{210 * 1024 * 1024, "210.0 MB/s"},
	} {
		if got := formatSpeed(tt.bps); got != tt.want {
			t.Errorf("formatSpeed(%v) = %q; want %q", tt.bps, got, tt.want)
		}
	}
}
