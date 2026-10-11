package api

import (
	"encoding/json"
	"net/http"
	"testing"

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
