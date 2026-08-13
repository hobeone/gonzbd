package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/api/apitest"
	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/queue"
)

// durabilitySlot is the part of the queue listing this file cares about,
// decoded by json tag so a rename breaks the test rather than silently
// zeroing the fields.
type durabilitySlot struct {
	NzoID           string `json:"nzo_id"`
	StallReason     string `json:"stall_reason"`
	BytesDurable    int64  `json:"bytes_durable"`
	BytesPending    int64  `json:"bytes_pending"`
	LastBarrierUnix int64  `json:"last_barrier_unix"`
}

// stallTestServer wires a queue and a NopApp whose checkpoint figures the
// caller controls, then returns both so a test can assert on the wire shape
// without standing up a real barrier.
func stallTestServer(t *testing.T, states map[string]app.JobCheckpointState, counter *atomic.Int64) (*Server, *queue.Queue) {
	t.Helper()
	q := queue.New()
	s := New(Options{
		Config:  &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}},
		Version: "1.0.0-test",
		Queue:   q,
		App: apitest.NopApp{
			Queue:               q,
			CheckpointStatesVal: states,
			ReevaluatedVal:      counter,
		},
	})
	return s, q
}

func queueDurabilitySlots(t *testing.T, s *Server, path string) []durabilitySlot {
	t.Helper()
	rr := apiGet(t, s.Handler(), path)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var got struct {
		Queue struct {
			Slots []durabilitySlot `json:"slots"`
		} `json:"queue"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got.Queue.Slots
}

func findDurabilitySlot(t *testing.T, slots []durabilitySlot, id string) durabilitySlot {
	t.Helper()
	for _, s := range slots {
		if s.NzoID == id {
			return s
		}
	}
	t.Fatalf("%s not present in the queue listing (%d slots)", id, len(slots))
	return durabilitySlot{}
}

// TestQueueAPI_ReportsStallReason pins R27 on the wire. A stalled job that
// carries no reason is indistinguishable from one that silently stopped, which
// is the difference between a full disk a user clears in ten seconds and a
// download they give up on.
func TestQueueAPI_ReportsStallReason(t *testing.T) {
	t.Parallel()
	s, q := stallTestServer(t, nil, nil)
	job := addTestJob(t, q, queue.AddOptions{Name: "stalled"})
	// Set after the job exists so the map key is the real ID.
	s.status = apitest.NopApp{Queue: q, CheckpointStatesVal: map[string]app.JobCheckpointState{
		job.ID: {StallReason: `Stalled: storage retryable fault on write "/data/x.bin": no space left on device`},
	}}

	slot := findDurabilitySlot(t, queueDurabilitySlots(t, s, "/api?mode=queue&apikey="+testAPIKey), job.ID)

	if slot.StallReason == "" {
		t.Fatal("stall_reason is empty for a stalled job — R27 requires an actionable reason")
	}
	if !strings.Contains(slot.StallReason, "no space") {
		t.Errorf("stall_reason = %q, does not name the condition", slot.StallReason)
	}
	if !strings.Contains(slot.StallReason, "/data/x.bin") {
		t.Errorf("stall_reason = %q, does not name the file — a job's files can sit on "+
			"different mounts, so the job name does not identify the device", slot.StallReason)
	}
}

// TestQueueAPI_OmitsStallReasonForAHealthyJob pins the other polarity, which is
// the one a UI branches on. A reason that is always present makes every row a
// warning row.
func TestQueueAPI_OmitsStallReasonForAHealthyJob(t *testing.T) {
	t.Parallel()
	s, q := stallTestServer(t, nil, nil)
	job := addTestJob(t, q, queue.AddOptions{Name: "healthy"})

	slot := findDurabilitySlot(t, queueDurabilitySlots(t, s, "/api?mode=queue&apikey="+testAPIKey), job.ID)

	if slot.StallReason != "" {
		t.Errorf("stall_reason = %q for a job that is not stalled, want empty", slot.StallReason)
	}
}

// TestQueueAPI_ReportsDurableAndPendingBytesSeparately pins R26.
//
// bytes_pending is what has been written but not yet covered by an fsync — the
// rework window made visible. It must be reported separately from
// bytes_durable and never summed into it: one figure survives a power loss and
// the other does not, so a total asserts the stronger claim about all of it.
func TestQueueAPI_ReportsDurableAndPendingBytesSeparately(t *testing.T) {
	t.Parallel()
	s, q := stallTestServer(t, nil, nil)
	job := addTestJob(t, q, queue.AddOptions{Name: "inflight"})
	barrierAt := time.Now().Truncate(time.Second)
	s.status = apitest.NopApp{Queue: q, CheckpointStatesVal: map[string]app.JobCheckpointState{
		job.ID: {PendingBytes: 512, LastBarrier: barrierAt},
	}}

	slot := findDurabilitySlot(t, queueDurabilitySlots(t, s, "/api?mode=queue&apikey="+testAPIKey), job.ID)

	if slot.BytesPending != 512 {
		t.Errorf("bytes_pending = %d, want 512 — the rework window is not reported at all",
			slot.BytesPending)
	}
	// The fixture's job has downloaded nothing, so anything non-zero here can
	// only be the pending bytes leaking into the durable figure.
	if slot.BytesDurable != 0 {
		t.Errorf("bytes_durable = %d before any barrier ran, want 0; the written-but-not-durable "+
			"bytes are being counted as durable, which claims they survive a power loss",
			slot.BytesDurable)
	}
	if slot.LastBarrierUnix != barrierAt.Unix() {
		t.Errorf("last_barrier_unix = %d, want %d", slot.LastBarrierUnix, barrierAt.Unix())
	}
}

// TestQueueAPI_ReportsNoLastBarrierAsZero pins the encoding of "never".
// time.Time's zero value renders as -6795364578871 through Unix(), which a
// client formats as a date in the year 1754 rather than as an absence.
func TestQueueAPI_ReportsNoLastBarrierAsZero(t *testing.T) {
	t.Parallel()
	s, q := stallTestServer(t, nil, nil)
	job := addTestJob(t, q, queue.AddOptions{Name: "fresh"})

	slot := findDurabilitySlot(t, queueDurabilitySlots(t, s, "/api?mode=queue&apikey="+testAPIKey), job.ID)

	if slot.LastBarrierUnix != 0 {
		t.Errorf("last_barrier_unix = %d for a job that has never checkpointed, want 0",
			slot.LastBarrierUnix)
	}
}

// TestQueueAPI_DetailCarriesTheSameDurabilityFields pins the drawer endpoint,
// which builds its slot through a separate call site. The UI opens it on the
// row that is stalled, so a field present in the listing and absent here is
// exactly the case a user hits.
func TestQueueAPI_DetailCarriesTheSameDurabilityFields(t *testing.T) {
	t.Parallel()
	s, q := stallTestServer(t, nil, nil)
	job := addTestJob(t, q, queue.AddOptions{Name: "drawer"})
	s.status = apitest.NopApp{Queue: q, CheckpointStatesVal: map[string]app.JobCheckpointState{
		job.ID: {StallReason: "Stalled: disk full", PendingBytes: 64},
	}}

	slots := queueDurabilitySlots(t, s,
		"/api?mode=queue&nzo_id="+job.ID+"&files=1&apikey="+testAPIKey)
	slot := findDurabilitySlot(t, slots, job.ID)

	if slot.StallReason == "" {
		t.Error("stall_reason is empty in the detail response; the drawer the user opens on a " +
			"stalled row is the one place the reason is missing")
	}
	if slot.BytesPending != 64 {
		t.Errorf("bytes_pending = %d in the detail response, want 64", slot.BytesPending)
	}
}

// TestQueueResume_AsksForAStallReevaluation pins R19's "on user action" half.
//
// Queue.Resume alone does not finish the completion a failed file finalize
// interrupted — nothing re-triggers a finalize for a file whose parts have all
// arrived, so without this the user presses resume, the row unpauses, and the
// job sits at 100% until the next interval or a restart.
func TestQueueResume_AsksForAStallReevaluation(t *testing.T) {
	t.Parallel()
	var reevaluated atomic.Int64
	s, q := stallTestServer(t, nil, &reevaluated)
	job := addTestJob(t, q, queue.AddOptions{Name: "resumed"})
	if err := q.Pause(job.ID); err != nil {
		t.Fatal(err)
	}

	rr := apiGet(t, s.Handler(), "/api?mode=queue&name=resume&value="+job.ID+"&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	if got := reevaluated.Load(); got == 0 {
		t.Error("resuming a job did not ask for a stall re-evaluation; a job parked by a " +
			"failed finalize stays at 100% until the next interval or a restart (R19)")
	}
}

// TestQueueResumeAll_AsksForAStallReevaluation pins the same for the
// queue-wide resume, which is the button a user reaches for after clearing a
// full disk.
func TestQueueResumeAll_AsksForAStallReevaluation(t *testing.T) {
	t.Parallel()
	var reevaluated atomic.Int64
	s, _ := stallTestServer(t, nil, &reevaluated)

	rr := apiGet(t, s.Handler(), "/api?mode=queue&name=resume_all&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	if got := reevaluated.Load(); got == 0 {
		t.Error("resume_all did not ask for a stall re-evaluation")
	}
}
