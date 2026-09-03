package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/api/apitest"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

type apiStubWorkers struct{ aborted []string }

func (s *apiStubWorkers) Abort(jobID string) { s.aborted = append(s.aborted, jobID) }

type apiStubResidency struct{}

func (r *apiStubResidency) Hydrate(ctx context.Context, id string) error { return nil }
func (r *apiStubResidency) Evict(id string)                              {}

type apiStubStore struct{}

func (s *apiStubStore) Load(ctx context.Context) ([]dispatch.Persisted, error) { return nil, nil }
func (s *apiStubStore) Save(ctx context.Context, p dispatch.Persisted) error   { return nil }
func (s *apiStubStore) Delete(ctx context.Context, id string) error            { return nil }

type apiStubRunner struct{}

func (r *apiStubRunner) Run(ctx context.Context, id string, state job.State) {}

func newTestAPIDispatcher(t *testing.T) *dispatch.Dispatcher {
	t.Helper()
	d := dispatch.New(2, 2, time.Hour, time.Now, &apiStubWorkers{}, &apiStubResidency{}, &apiStubStore{}, &apiStubRunner{})
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("dispatcher.Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	return d
}

func testDispatcherServer(t *testing.T, d *dispatch.Dispatcher, cats ...config.CategoryConfig) *Server {
	t.Helper()
	cfg := &config.Config{
		General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey},
		Categories: []config.CategoryConfig{
			{Name: "Default", PP: 3, Priority: 0, Script: "None"},
			{Name: "tv", PP: 2, Priority: 1, Script: "tv.sh"},
		},
	}
	if len(cats) > 0 {
		cfg.Categories = cats
	}
	s := New(Options{
		Config:     cfg,
		Version:    "1.0.0-test",
		Dispatcher: d,
		App:        apitest.NopApp{Dispatcher: d},
	})
	return s
}

func TestQueueList_Dispatcher(t *testing.T) {
	t.Parallel()
	d := newTestAPIDispatcher(t)
	s := testDispatcherServer(t, d)

	j1 := job.New("j1", "Ubuntu 22.04", job.Policy{})
	h1 := dispatch.Header{
		Name:     "Ubuntu 22.04",
		Filename: "ubuntu.nzb",
		Category: "Default",
		Priority: 0,
		Bytes:    1000,
	}
	if err := d.Add(j1, h1); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}

	j2 := job.New("j2", "Debian 12", job.Policy{})
	h2 := dispatch.Header{
		Name:     "Debian 12",
		Filename: "debian.nzb",
		Category: "tv",
		Priority: 1,
		Bytes:    2000,
	}
	if err := d.Add(j2, h2); err != nil {
		t.Fatalf("Add(j2): %v", err)
	}

	// 1. List all
	w := apiGet(t, s.Handler(), "/api?mode=queue&output=json&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	var resp queueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if !resp.Status {
		t.Errorf("status = false; want true")
	}
	if len(resp.Queue.Slots) != 2 {
		t.Fatalf("slots = %d; want 2", len(resp.Queue.Slots))
	}
	if resp.Queue.Slots[0].NzoID != "j1" || resp.Queue.Slots[1].NzoID != "j2" {
		t.Errorf("slots order = [%s, %s]; want [j1, j2]", resp.Queue.Slots[0].NzoID, resp.Queue.Slots[1].NzoID)
	}

	// 2. Filter by cat=tv
	w = apiGet(t, s.Handler(), "/api?mode=queue&output=json&cat=tv&apikey="+testAPIKey)
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if len(resp.Queue.Slots) != 1 || resp.Queue.Slots[0].NzoID != "j2" {
		t.Errorf("filtered slots = %v; want [j2]", resp.Queue.Slots)
	}

	// 3. Search filter
	w = apiGet(t, s.Handler(), "/api?mode=queue&output=json&search=debian&apikey="+testAPIKey)
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if len(resp.Queue.Slots) != 1 || resp.Queue.Slots[0].NzoID != "j2" {
		t.Errorf("search slots = %v; want [j2]", resp.Queue.Slots)
	}
}

func TestQueuePauseAndResume_Dispatcher(t *testing.T) {
	t.Parallel()
	d := newTestAPIDispatcher(t)
	s := testDispatcherServer(t, d)

	j1 := job.New("j1", "Ubuntu 22.04", job.Policy{})
	if err := d.Add(j1, dispatch.Header{Name: "Ubuntu 22.04", Bytes: 1000}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}

	// 1. Pause job j1
	w := apiGet(t, s.Handler(), "/api?mode=queue&name=pause&value=j1&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	row, ok := d.Row("j1")
	if !ok || row.Status() != constants.StatusPaused {
		t.Errorf("j1 status = %v; want Paused", row.Status())
	}

	// 2. Resume job j1
	w = apiGet(t, s.Handler(), "/api?mode=queue&name=resume&value=j1&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	row, ok = d.Row("j1")
	if !ok || row.Status() == constants.StatusPaused {
		t.Errorf("j1 status after resume = %v; want not paused", row.Status())
	}

	// 3. Pause all via mode=pause
	w = apiGet(t, s.Handler(), "/api?mode=pause&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if !d.Paused() {
		t.Errorf("dispatcher.Paused() = false; want true")
	}

	// 4. Resume all via mode=resume
	w = apiGet(t, s.Handler(), "/api?mode=resume&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if d.Paused() {
		t.Errorf("dispatcher.Paused() = true; want false")
	}

	// 5. Pause all via mode=queue&name=pause_all
	w = apiGet(t, s.Handler(), "/api?mode=queue&name=pause_all&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if !d.Paused() {
		t.Errorf("dispatcher.Paused() = false; want true after pause_all")
	}

	// 6. Resume all via mode=queue&name=resume_all
	w = apiGet(t, s.Handler(), "/api?mode=queue&name=resume_all&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if d.Paused() {
		t.Errorf("dispatcher.Paused() = true; want false after resume_all")
	}
}

func TestQueueMutators_Dispatcher(t *testing.T) {
	t.Parallel()
	d := newTestAPIDispatcher(t)
	s := testDispatcherServer(t, d)

	j1 := job.New("j1", "Test Job", job.Policy{})
	if err := d.Add(j1, dispatch.Header{Name: "Test Job", Bytes: 1000}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}

	// 1. Priority
	w := apiGet(t, s.Handler(), "/api?mode=queue&name=priority&value=j1&value2=2&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	row, _ := d.Row("j1")
	if row.Header.Priority != 2 {
		t.Errorf("Priority = %d; want 2", row.Header.Priority)
	}

	// 2. Change opts (PP)
	w = apiGet(t, s.Handler(), "/api?mode=queue&name=change_opts&value=j1&value2=3&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	row, _ = d.Row("j1")
	if row.Header.PP != 3 || !j1.Policy().Delete {
		t.Errorf("PP = %d, Policy = %+v; want PP 3", row.Header.PP, j1.Policy())
	}

	// 3. Change cat
	w = apiGet(t, s.Handler(), "/api?mode=queue&name=change_cat&value=j1&value2=tv&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	row, _ = d.Row("j1")
	if row.Header.Category != "tv" || row.Header.PP != 2 || row.Header.Script != "tv.sh" || row.Header.Priority != 1 {
		t.Errorf("Cat = %q, PP = %d, Script = %q, Priority = %d; want tv, 2, tv.sh, 1",
			row.Header.Category, row.Header.PP, row.Header.Script, row.Header.Priority)
	}

	// 4. Rename
	w = apiGet(t, s.Handler(), "/api?mode=queue&name=rename&value=j1&value2=Renamed&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	row, _ = d.Row("j1")
	if row.Header.Name != "Renamed" || j1.Name() != "Renamed" {
		t.Errorf("Name = %q / %q; want Renamed", row.Header.Name, j1.Name())
	}

	// 5. Change script
	w = apiGet(t, s.Handler(), "/api?mode=queue&name=change_script&value=j1&value2=custom.sh&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	row, _ = d.Row("j1")
	if row.Header.Script != "custom.sh" {
		t.Errorf("Script = %q; want custom.sh", row.Header.Script)
	}
}

func TestQueueDelete_Dispatcher(t *testing.T) {
	t.Parallel()
	d := newTestAPIDispatcher(t)
	s := testDispatcherServer(t, d)

	j1 := job.New("j1", "Job 1", job.Policy{})
	j2 := job.New("j2", "Job 2", job.Policy{})
	_ = d.Add(j1, dispatch.Header{Name: "Job 1"})
	_ = d.Add(j2, dispatch.Header{Name: "Job 2"})

	// Delete j1
	w := apiGet(t, s.Handler(), "/api?mode=queue&name=delete&value=j1&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if d.Len() != 1 {
		t.Fatalf("d.Len() = %d; want 1", d.Len())
	}

	// Purge all remaining
	w = apiGet(t, s.Handler(), "/api?mode=queue&name=purge&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if d.Len() != 0 {
		t.Fatalf("d.Len() = %d; want 0 after purge", d.Len())
	}
}

func TestStatus_Dispatcher(t *testing.T) {
	t.Parallel()
	d := newTestAPIDispatcher(t)
	s := testDispatcherServer(t, d)

	j1 := job.New("j1", "Job 1", job.Policy{})
	_ = d.Add(j1, dispatch.Header{Name: "Job 1"})

	w := apiGet(t, s.Handler(), "/api?mode=fullstatus&output=json&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	var resp struct {
		Status struct {
			Paused    bool `json:"paused"`
			NoOfSlots int  `json:"noofslots"`
		} `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if resp.Status.Paused {
		t.Errorf("paused = true; want false")
	}
	if resp.Status.NoOfSlots != 1 {
		t.Errorf("noofslots = %d; want 1", resp.Status.NoOfSlots)
	}
}
