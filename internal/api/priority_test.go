package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/urlgrabber"
)

// priorityTestCats gives "tv" a priority of its own, so a request that
// resolves through the job's category is distinguishable from one that falls
// to the Default category or to Normal.
func priorityTestCats() []config.CategoryConfig {
	return []config.CategoryConfig{
		{Name: "Default", PP: 3, Priority: int(constants.LowPriority)},
		{Name: "tv", PP: 3, Priority: int(constants.HighPriority)},
	}
}

func setPriority(t *testing.T, s *Server, id, value2 string) *httptest.ResponseRecorder {
	t.Helper()
	return apiGet(t, s.Handler(),
		"/api?mode=queue&name=priority&value="+id+"&value2="+value2+"&apikey="+testAPIKey)
}

func jobIntent(t *testing.T, q *testQueueWrapper, id string) job.Intent {
	t.Helper()
	j, ok := q.disp.Job(id)
	if !ok {
		t.Fatalf("Job(%s): not registered", id)
	}
	return j.Intent()
}

// TestQueuePriority_PausedPausesJob pins that value2=-2 pauses the job as
// ingest does, and stores the priority its category resolves to rather than
// the -2 sentinel, which would sort it behind Low.
func TestQueuePriority_PausedPausesJob(t *testing.T) {
	t.Parallel()
	s, q := testQueueServerWithCats(t, priorityTestCats())
	jb := addTestJob(t, q, testAddOptions{Filename: "job.nzb", Category: "tv"})

	rr := setPriority(t, s, jb.ID, "-2")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := jobIntent(t, q, jb.ID); got != job.IntentPause {
		t.Errorf("Intent = %v; want %v", got, job.IntentPause)
	}
	if got := q.SnapshotJob(jb.ID).Priority; got != constants.HighPriority {
		t.Errorf("Priority = %s; want High (tv's, in place of the Paused sentinel)", got)
	}
}

// TestQueuePriority_DefaultResolvesThroughCategory pins that value2=-100
// resolves through the job's category as ingest does, instead of storing -100.
func TestQueuePriority_DefaultResolvesThroughCategory(t *testing.T) {
	t.Parallel()
	s, q := testQueueServerWithCats(t, priorityTestCats())
	jb := addTestJob(t, q, testAddOptions{Filename: "job.nzb", Category: "tv"})

	rr := setPriority(t, s, jb.ID, "-100")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := q.SnapshotJob(jb.ID).Priority; got != constants.HighPriority {
		t.Errorf("Priority = %s; want High (tv's)", got)
	}
	if got := jobIntent(t, q, jb.ID); got != job.IntentRun {
		t.Errorf("Intent = %v; want %v: Default does not pause", got, job.IntentRun)
	}
}

// TestQueuePriority_RefusesNonPriorities pins that Stop and values outside the
// defined set answer 400 and leave the job's priority as it was.
func TestQueuePriority_RefusesNonPriorities(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"3", "-4", "5", "7", "-3", "300", "-129", "abc"} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			s, q := testQueueServer(t)
			jb := addTestJob(t, q, testAddOptions{Filename: "job.nzb"})

			rr := setPriority(t, s, jb.ID, v)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 (body: %s)", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "priority") {
				t.Errorf("body = %q; want it to name the priority", rr.Body.String())
			}
			if got := q.SnapshotJob(jb.ID).Priority; got != constants.NormalPriority {
				t.Errorf("Priority = %s; want Normal, unchanged", got)
			}
			if got := jobIntent(t, q, jb.ID); got != job.IntentRun {
				t.Errorf("Intent = %v; want %v, unchanged", got, job.IntentRun)
			}
		})
	}
}

// TestQueuePriority_ReportsListingPosition pins that the response's position
// is the job's index in the priority-ordered queue listing after the change.
func TestQueuePriority_ReportsListingPosition(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	a := addTestJob(t, q, testAddOptions{Filename: "a.nzb"})
	_ = addTestJob(t, q, testAddOptions{Filename: "b.nzb"})
	c := addTestJob(t, q, testAddOptions{Filename: "c.nzb"})

	for _, step := range []struct {
		id, value2 string
		want       int
	}{
		{c.ID, strconv.Itoa(int(constants.ForcePriority)), 0}, // was 2
		{a.ID, strconv.Itoa(int(constants.LowPriority)), 2},   // was 1 (behind c)
	} {
		rr := setPriority(t, s, step.id, step.value2)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		var resp struct {
			Position int `json:"position"`
		}
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Position != step.want {
			t.Errorf("value2=%s on %s: position = %d; want %d", step.value2, step.id, resp.Position, step.want)
		}
	}
}

func addFileWithPriority(t *testing.T, s *Server, cat, priority string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("nzbfile", "test.nzb")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(makeTestNZB(t)); err != nil {
		t.Fatalf("write nzb: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost,
		"/api?mode=addfile&cat="+cat+"&priority="+priority+"&apikey="+testAPIKey, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

// TestAddFile_PausedPriority pins that priority=-2 on an add pauses the job and
// stores the category's priority, through the same resolver as name=priority.
func TestAddFile_PausedPriority(t *testing.T) {
	t.Parallel()
	s, q := testQueueServerWithCats(t, priorityTestCats())

	rr := addFileWithPriority(t, s, "tv", "-2")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		NzoIDs []string `json:"nzo_ids"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.NzoIDs) != 1 {
		t.Fatalf("nzo_ids = %v; want one", resp.NzoIDs)
	}
	id := resp.NzoIDs[0]
	if got := jobIntent(t, q, id); got != job.IntentPause {
		t.Errorf("Intent = %v; want %v", got, job.IntentPause)
	}
	if got := q.SnapshotJob(id).Priority; got != constants.HighPriority {
		t.Errorf("Priority = %s; want High (tv's, in place of the Paused sentinel)", got)
	}
}

// TestAddFile_RefusesNonPriorities pins that addfile (and addlocalfile, which
// shares enqueueNZBData) answers 400 for Stop or an undefined value and adds
// no job.
func TestAddFile_RefusesNonPriorities(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"3", "-4", "7", "300", "abc"} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			s, _ := testQueueServer(t)
			rr := addFileWithPriority(t, s, "", v)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 (body: %s)", rr.Code, rr.Body.String())
			}
			if n := s.Dispatcher().Len(); n != 0 {
				t.Errorf("queue len = %d; want 0", n)
			}
		})
	}
}

// TestAddURL_RefusesNonPriority pins that addurl refuses Stop and Repair before
// fetching: the URL names nothing that answers, so a fetch would be a 502.
func TestAddURL_RefusesNonPriority(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"-4", "3"} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			s, _ := testQueueServer(t)
			s.grabber = urlgrabber.New(urlgrabber.Config{AllowPrivateIPs: true}, mockGrabberHandler{id: "nzo_test1"})

			rr := apiGet(t, s.Handler(),
				"/api?mode=addurl&name=http%3A%2F%2F127.0.0.1%3A1%2Fx.nzb&priority="+v+"&apikey="+testAPIKey)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 (body: %s)", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestQueueChangeCat_PausedCategoryPauses pins that change_cat to a category
// whose priority is Paused pauses the job and stores the fallback category's
// priority, through the same resolver as name=priority.
func TestQueueChangeCat_PausedCategoryPauses(t *testing.T) {
	t.Parallel()
	cats := []config.CategoryConfig{
		{Name: "Default", PP: 3, Priority: int(constants.LowPriority)},
		{Name: "hold", PP: 3, Priority: int(constants.PausedPriority)},
	}
	s, q := testQueueServerWithCats(t, cats)
	jb := addTestJob(t, q, testAddOptions{Filename: "job.nzb", Category: "Default"})

	rr := apiGet(t, s.Handler(),
		"/api?mode=queue&name=change_cat&value="+jb.ID+"&value2=hold&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := jobIntent(t, q, jb.ID); got != job.IntentPause {
		t.Errorf("Intent = %v; want %v", got, job.IntentPause)
	}
	if got := q.SnapshotJob(jb.ID).Priority; got != constants.LowPriority {
		t.Errorf("Priority = %s; want Low (the Default category's)", got)
	}
}
