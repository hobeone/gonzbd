package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hobeone/gonzbd/internal/api/apitest"
	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/buildinfo"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
)

// durabilitySlot is the part of the queue listing this file cares about,
// decoded by json tag so a rename breaks the test rather than silently
// zeroing the fields.
type durabilitySlot struct {
	NzoID        string `json:"nzo_id"`
	StallReason  string `json:"stall_reason"`
	BytesDurable int64  `json:"bytes_durable"`
}

// stallTestServer wires a dispatcher and a NopApp whose stall figures the
// caller controls, then returns both so a test can assert on the wire shape
// without standing up a recorder.
func stallTestServer(t *testing.T, states map[string]app.JobCheckpointState, counter *atomic.Int64) (*Server, *dispatch.Dispatcher) {
	t.Helper()
	disp := newTestAPIDispatcher(t)
	s := New(Options{
		Config:     &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}},
		Build:      buildinfo.Info{Version: "1.0.0-test"},
		Dispatcher: disp,
		App: apitest.NopApp{
			CheckpointStatesVal: states,
			ReevaluatedVal:      counter,
		},
	})
	return s, disp
}

func addTestDispatcherJob(t *testing.T, disp *dispatch.Dispatcher, name string) *job.Job {
	t.Helper()
	files := []job.JobFile{
		{
			Subject:  name + ".rar",
			Bytes:    1024,
			Articles: []job.JobArticle{{ID: name + "@t", Bytes: 1024, Number: 1}},
		},
	}
	m := job.NewManifest(files)
	j := job.New("job-"+name, name, job.Policy{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if err := disp.Add(context.Background(), j, dispatch.Header{
		Name:     name,
		Filename: name + ".nzb",
		Bytes:    m.TotalBytes(),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return j
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
	s, disp := stallTestServer(t, nil, nil)
	j := addTestDispatcherJob(t, disp, "stalled")
	// Set after the job exists so the map key is the real ID.
	s.status = apitest.NopApp{CheckpointStatesVal: map[string]app.JobCheckpointState{
		j.ID(): {StallReason: `Stalled: storage retryable fault on write "/data/x.bin": no space left on device`},
	}}

	slot := findDurabilitySlot(t, queueDurabilitySlots(t, s, "/api?mode=queue&apikey="+testAPIKey), j.ID())

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

// TestQueueAPI_AlwaysSendsStallReasonEvenWhenEmpty pins the wire shape the
// brief specifies: stall_reason is present on every slot, empty when the job
// is not parked.
//
// It replaces a test that was named for omitting the field and asserted the
// opposite — and could assert nothing either way, because a decoded struct
// yields "" whether the key was absent or present-and-empty. The distinction
// is what lets a client tell an unstalled job from a server too old to send
// the field, so the assertion has to be made against the raw object.
func TestQueueAPI_AlwaysSendsStallReasonEvenWhenEmpty(t *testing.T) {
	t.Parallel()
	s, disp := stallTestServer(t, nil, nil)
	j := addTestDispatcherJob(t, disp, "healthy")

	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var raw struct {
		Queue struct {
			Slots []map[string]any `json:"slots"`
		} `json:"queue"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Queue.Slots) != 1 {
		t.Fatalf("slots = %d, want 1", len(raw.Queue.Slots))
	}
	slot := raw.Queue.Slots[0]
	if slot["nzo_id"] != j.ID() {
		t.Fatalf("slot nzo_id = %v, want %s", slot["nzo_id"], j.ID())
	}
	got, present := slot["stall_reason"]
	if !present {
		t.Fatal("stall_reason is absent from a healthy job's slot; a client cannot tell that " +
			"from a server that does not implement the field at all")
	}
	if got != "" {
		t.Errorf("stall_reason = %v for a job that is not stalled, want empty", got)
	}
}

// TestQueueAPI_ReportsDurableBytesFromRecordedRuns pins bytes_durable on the
// wire: it is zero before any run is recorded and follows the recorded runs.
func TestQueueAPI_ReportsDurableBytesFromRecordedRuns(t *testing.T) {
	t.Parallel()
	s, disp := stallTestServer(t, nil, nil)
	j := addTestDispatcherJob(t, disp, "inflight")

	before := findDurabilitySlot(t, queueDurabilitySlots(t, s, "/api?mode=queue&apikey="+testAPIKey), j.ID())
	if before.BytesDurable != 0 {
		t.Errorf("bytes_durable = %d before any article was written, want 0", before.BytesDurable)
	}

	// Now make the job's single 1024-byte article Done, through the door a
	// resume's verified rows enter by. Asserting only the zero above pinned
	// nothing: a bytes_durable that always answered 0 satisfied it.
	if _, err := j.InstallVerified(0, []durability.WrittenRow{
		{FileIdx: 0, ArtIdx: 0, Length: 1024},
	}); err != nil {
		t.Fatalf("InstallVerified: %v", err)
	}

	after := findDurabilitySlot(t, queueDurabilitySlots(t, s, "/api?mode=queue&apikey="+testAPIKey), j.ID())
	if after.BytesDurable != 1024 {
		t.Errorf("bytes_durable = %d after a recorded run covered the job's only article, "+
			"want 1024 — the field reports nothing the written rows achieved", after.BytesDurable)
	}
}

// TestQueueAPI_DetailCarriesTheSameDurabilityFields pins the drawer endpoint,
// which builds its slot through a separate call site. The UI opens it on the
// row that is stalled, so a field present in the listing and absent here is
// exactly the case a user hits.
func TestQueueAPI_DetailCarriesTheSameDurabilityFields(t *testing.T) {
	t.Parallel()
	s, disp := stallTestServer(t, nil, nil)
	j := addTestDispatcherJob(t, disp, "drawer")
	s.status = apitest.NopApp{CheckpointStatesVal: map[string]app.JobCheckpointState{
		j.ID(): {StallReason: "Stalled: disk full"},
	}}

	slots := queueDurabilitySlots(t, s,
		"/api?mode=queue&nzo_id="+j.ID()+"&files=1&apikey="+testAPIKey)
	slot := findDurabilitySlot(t, slots, j.ID())

	if slot.StallReason == "" {
		t.Error("stall_reason is empty in the detail response; the drawer the user opens on a " +
			"stalled row is the one place the reason is missing")
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
	s, disp := stallTestServer(t, nil, &reevaluated)
	j := addTestDispatcherJob(t, disp, "resumed")
	if err := disp.PauseJob(j.ID()); err != nil {
		t.Fatal(err)
	}

	rr := apiGet(t, s.Handler(), "/api?mode=queue&name=resume&value="+j.ID()+"&apikey="+testAPIKey)
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
