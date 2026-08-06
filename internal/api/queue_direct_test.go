package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/urlgrabber"
)

// This file directly invokes the unexported queue.go handlers/helpers (not
// merely through s.Handler()'s mode=queue dispatch) so the behaviour they
// implement is pinned even if modeQueue's routing table changes. Coverage of
// the same behaviour also exists at the HTTP-dispatch layer elsewhere in
// this package (queue_test.go); these tests intentionally overlap that in
// order to satisfy check_test_alignment's direct-reference requirement
// without weakening either layer.

// --- sanitizeScriptParam ---

func TestSanitizeScriptParam(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, input, want string
	}{
		{"empty stays empty", "", ""},
		{"none is preserved verbatim", "NoNe", "NoNe"},
		{"default is preserved verbatim", "DEFAULT", "DEFAULT"},
		{"strips unix directory components", "/etc/scripts/evil.sh", "evil.sh"},
		{"strips relative traversal", "../../evil.sh", "evil.sh"},
		{"converts backslashes before basing", `C:\scripts\test.sh`, "test.sh"},
		{"plain name is unchanged", "post.sh", "post.sh"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sanitizeScriptParam(tt.input); got != tt.want {
				t.Errorf("sanitizeScriptParam(%q) = %q; want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- firstIncompleteFile ---

func TestFirstIncompleteFile(t *testing.T) {
	t.Parallel()

	t.Run("all files complete returns empty", func(t *testing.T) {
		t.Parallel()
		_, q := testQueueServer(t)
		job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})
		if err := q.MarkArticlesDone(job.ID, []string{"test-article-id-001@example.com"}); err != nil {
			t.Fatalf("MarkArticlesDone: %v", err)
		}
		if err := q.MarkFileComplete(job.ID, 0); err != nil {
			t.Fatalf("MarkFileComplete: %v", err)
		}
		snap := q.SnapshotJob(job.ID)
		if got := firstIncompleteFile(snap); got != "" {
			t.Errorf("firstIncompleteFile = %q; want empty when every file is complete", got)
		}
	})

	t.Run("returns the first incomplete file's subject", func(t *testing.T) {
		t.Parallel()
		parsed := &nzb.NZB{Files: []nzb.File{
			{Subject: "file0.rar", Bytes: 500, Articles: []nzb.Article{{ID: "a0@t", Bytes: 500, Number: 1}}},
			{Subject: "file1.rar", Bytes: 500, Articles: []nzb.Article{{ID: "a1@t", Bytes: 500, Number: 1}}},
		}}
		job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "f.nzb"}, fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatalf("NewJob: %v", err)
		}
		q := queue.New()
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if err := q.MarkArticlesDone(job.ID, []string{"a0@t"}); err != nil {
			t.Fatalf("MarkArticlesDone: %v", err)
		}
		if err := q.MarkFileComplete(job.ID, 0); err != nil {
			t.Fatalf("MarkFileComplete: %v", err)
		}
		snap := q.SnapshotJob(job.ID)
		if got := firstIncompleteFile(snap); got != "file1.rar" {
			t.Errorf("firstIncompleteFile = %q; want file1.rar", got)
		}
	})

	t.Run("non-resident job returns empty rather than erroring", func(t *testing.T) {
		t.Parallel()
		job := newEvictedJob(t)
		if got := firstIncompleteFile(job); got != "" {
			t.Errorf("firstIncompleteFile = %q; want empty for a non-resident job", got)
		}
	})
}

// --- buildQueueFiles ---

func TestBuildQueueFiles(t *testing.T) {
	t.Parallel()

	t.Run("returns the per-file breakdown", func(t *testing.T) {
		t.Parallel()
		_, q := testQueueServer(t)
		job := addLargeTestJob(t, q, 4) // 4 segments x 1 MiB
		jobInternal, err := q.Get(job.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		m := mustManifest(t, jobInternal)
		doneIDs := []string{m.ArticleID(0), m.ArticleID(1)}
		if err := q.MarkArticlesDone(job.ID, doneIDs); err != nil {
			t.Fatalf("MarkArticlesDone: %v", err)
		}
		snap := q.SnapshotJob(job.ID)

		files := buildQueueFiles(snap)
		if len(files) != 1 {
			t.Fatalf("len(files) = %d; want 1", len(files))
		}
		f := files[0]
		if f.Bytes != 4*1024*1024 {
			t.Errorf("Bytes = %d; want %d", f.Bytes, 4*1024*1024)
		}
		if f.BytesDownloaded != 2*1024*1024 {
			t.Errorf("BytesDownloaded = %d; want %d", f.BytesDownloaded, 2*1024*1024)
		}
		if f.State != "downloading" {
			t.Errorf("State = %q; want downloading", f.State)
		}
		if f.Name == "" {
			t.Error("Name should not be empty")
		}
	})

	t.Run("non-resident job returns a non-nil empty slice", func(t *testing.T) {
		t.Parallel()
		job := newEvictedJob(t)
		got := buildQueueFiles(job)
		if got == nil {
			t.Fatal("buildQueueFiles returned nil; want a non-nil empty slice so JSON encodes as []")
		}
		if len(got) != 0 {
			t.Errorf("len(got) = %d; want 0 for a non-resident job", len(got))
		}
	})
}

// --- queuePauseAll / queueResumeAll ---

func TestQueuePauseAll_Direct(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	s.queuePauseAll(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !q.IsPaused() {
		t.Error("queue should be paused after queuePauseAll")
	}
	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status field = %v; want true", m["status"])
	}
}

func TestQueueResumeAll_Direct(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	q.PauseAll()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	s.queueResumeAll(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if q.IsPaused() {
		t.Error("queue should not be paused after queueResumeAll")
	}
}

// --- trivial stub responders ---

func TestQueueNotImplemented_Direct(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)
	rr := httptest.NewRecorder()
	s.queueNotImplemented(rr, "sort")

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "sort") {
		t.Errorf("body = %q; want it to name the unimplemented action", rr.Body.String())
	}
}

func TestQueueChangeCompleteAction_Direct(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	s.queueChangeCompleteAction(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status field = %v; want true", m["status"])
	}
}

func TestQueueUnknownAction_Direct(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)
	rr := httptest.NewRecorder()
	s.queueUnknownAction(rr, "frobnicate")

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "frobnicate") {
		t.Errorf("body = %q; want it to name the unknown action", rr.Body.String())
	}
}

// --- queueDelete / queuePurge ---

func TestQueueDelete_Direct(t *testing.T) {
	t.Parallel()

	t.Run("removes the matching job", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})

		req := httptest.NewRequest(http.MethodGet, "/?value="+job.ID, nil)
		rr := httptest.NewRecorder()
		s.queueDelete(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		if q.Len() != 0 {
			t.Errorf("queue len = %d; want 0 after delete", q.Len())
		}
		var resp struct {
			Status bool     `json:"status"`
			NzoIDs []string `json:"nzo_ids"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.NzoIDs) != 1 || resp.NzoIDs[0] != job.ID {
			t.Errorf("nzo_ids = %v; want [%s]", resp.NzoIDs, job.ID)
		}
	})

	t.Run("missing value parameter is a 400", func(t *testing.T) {
		t.Parallel()
		s, _ := testQueueServer(t)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		s.queueDelete(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d; want 400", rr.Code)
		}
	})
}

func TestQueuePurge_Direct(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addTestJob(t, q, queue.AddOptions{Filename: "a.nzb"})
	addTestJob(t, q, queue.AddOptions{Filename: "b.nzb"})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	s.queuePurge(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if q.Len() != 0 {
		t.Errorf("queue len = %d; want 0 after purge", q.Len())
	}
}

// --- queueSetPaused / queueResumeJobs ---

func TestQueueSetPaused_Direct(t *testing.T) {
	t.Parallel()

	t.Run("pauses matching jobs and logs the found one", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})

		// A nonexistent ID is included alongside a real one to exercise the
		// silently-ignored not-found branch without failing the request.
		req := httptest.NewRequest(http.MethodGet, "/?value="+job.ID+",nonexistent", nil)
		rr := httptest.NewRecorder()
		s.queueSetPaused(rr, req, s.queue.Pause, "paused")

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		updated, err := q.Get(job.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if updated.Status != constants.StatusPaused {
			t.Errorf("Status = %q; want %q", updated.Status, constants.StatusPaused)
		}
	})

	t.Run("missing value parameter is a 400", func(t *testing.T) {
		t.Parallel()
		s, _ := testQueueServer(t)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		s.queueSetPaused(rr, req, s.queue.Pause, "paused")

		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d; want 400", rr.Code)
		}
	})
}

func TestQueuePauseJobs_Direct(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})

	// A nonexistent ID is included alongside a real one, mirroring
	// queueSetPaused's lenient bulk semantics (not-found IDs are silently
	// ignored rather than failing the whole request).
	req := httptest.NewRequest(http.MethodGet, "/?value="+job.ID+",nonexistent", nil)
	rr := httptest.NewRecorder()
	s.queuePauseJobs(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	updated, err := q.Get(job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Status != constants.StatusPaused {
		t.Errorf("Status = %q; want %q", updated.Status, constants.StatusPaused)
	}
}

func TestQueueResumeJobs_Direct(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})
	if err := q.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/?value="+job.ID, nil)
	rr := httptest.NewRecorder()
	s.queueResumeJobs(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	updated, err := q.Get(job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Status == constants.StatusPaused {
		t.Error("job should no longer be paused after queueResumeJobs")
	}
}

// --- queuePriority ---

func TestQueuePriority_Direct(t *testing.T) {
	t.Parallel()

	t.Run("sets the job priority", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})

		req := httptest.NewRequest(http.MethodGet,
			"/?value="+job.ID+"&value2="+"1", nil)
		rr := httptest.NewRecorder()
		s.queuePriority(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		updated, err := q.Get(job.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if updated.Priority != constants.HighPriority {
			t.Errorf("Priority = %v; want HighPriority", updated.Priority)
		}
	})

	t.Run("unknown job is a 404", func(t *testing.T) {
		t.Parallel()
		s, _ := testQueueServer(t)
		req := httptest.NewRequest(http.MethodGet, "/?value=nonexistent&value2=1", nil)
		rr := httptest.NewRecorder()
		s.queuePriority(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Errorf("status = %d; want 404", rr.Code)
		}
	})

	t.Run("missing value2 is a 400", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})
		req := httptest.NewRequest(http.MethodGet, "/?value="+job.ID, nil)
		rr := httptest.NewRecorder()
		s.queuePriority(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d; want 400", rr.Code)
		}
	})
}

// --- queueChangeOpts ---

func TestQueueChangeOpts_Direct(t *testing.T) {
	t.Parallel()

	t.Run("sets the PP level", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})

		req := httptest.NewRequest(http.MethodGet, "/?value="+job.ID+"&value2=3", nil)
		rr := httptest.NewRecorder()
		s.queueChangeOpts(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		updated, err := q.Get(job.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if updated.PP != 3 {
			t.Errorf("PP = %d; want 3", updated.PP)
		}
	})

	t.Run("out-of-range pp is a 400", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})
		req := httptest.NewRequest(http.MethodGet, "/?value="+job.ID+"&value2=5", nil)
		rr := httptest.NewRecorder()
		s.queueChangeOpts(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d; want 400", rr.Code)
		}
	})

	// Both edges of `pp < 0 || pp > 3`, one value either side of each.
	// Testing only an interior value and one well outside the range leaves
	// both comparisons free to move by one without any test noticing: with
	// only pp=3 and pp=5, widening the upper bound to `pp > 4` still rejects
	// 5, and no negative value at all leaves `pp < 0` unpinned entirely.
	for _, tc := range []struct {
		name string
		pp   string
		want int
	}{
		{"lower boundary is valid", "0", http.StatusOK},
		{"one below the lower boundary is a 400", "-1", http.StatusBadRequest},
		{"one above the upper boundary is a 400", "4", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, q := testQueueServer(t)
			job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})
			req := httptest.NewRequest(http.MethodGet, "/?value="+job.ID+"&value2="+tc.pp, nil)
			rr := httptest.NewRecorder()
			s.queueChangeOpts(rr, req)

			if rr.Code != tc.want {
				t.Fatalf("status = %d; want %d (body: %s)", rr.Code, tc.want, rr.Body.String())
			}
			if tc.want != http.StatusOK {
				return
			}
			updated, err := q.Get(job.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if updated.PP != 0 {
				t.Errorf("PP = %d; want 0", updated.PP)
			}
		})
	}

	t.Run("unknown job is a 404", func(t *testing.T) {
		t.Parallel()
		s, _ := testQueueServer(t)
		req := httptest.NewRequest(http.MethodGet, "/?value=nonexistent&value2=2", nil)
		rr := httptest.NewRecorder()
		s.queueChangeOpts(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Errorf("status = %d; want 404", rr.Code)
		}
	})
}

// --- queueChangeCat ---

func TestQueueChangeCat_Direct(t *testing.T) {
	t.Parallel()

	t.Run("applies the category's inherited settings", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServerWithCats(t, []config.CategoryConfig{
			{Name: "Default", PP: 3, Script: "", Priority: int(constants.NormalPriority)},
			{Name: "movies", PP: 2, Script: "movies.sh", Priority: int(constants.HighPriority)},
		})
		job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb", Category: "Default"})

		req := httptest.NewRequest(http.MethodGet, "/?value="+job.ID+"&value2=movies", nil)
		rr := httptest.NewRecorder()
		s.queueChangeCat(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		updated, err := q.Get(job.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if updated.Category != "movies" || updated.PP != 2 || updated.Script != "movies.sh" {
			t.Errorf("job = %+v; want category=movies pp=2 script=movies.sh", updated)
		}
	})

	t.Run("unknown job is a 404", func(t *testing.T) {
		t.Parallel()
		s, _ := testQueueServerWithCats(t, nil)
		req := httptest.NewRequest(http.MethodGet, "/?value=nonexistent&value2=movies", nil)
		rr := httptest.NewRecorder()
		s.queueChangeCat(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Errorf("status = %d; want 404", rr.Code)
		}
	})
}

// --- queueChangeName ---

func TestQueueChangeName_Direct(t *testing.T) {
	t.Parallel()

	t.Run("renames the job", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		job := addTestJob(t, q, queue.AddOptions{Filename: "original.nzb"})

		req := httptest.NewRequest(http.MethodGet, "/?value="+job.ID+"&value2=NewName", nil)
		rr := httptest.NewRecorder()
		s.queueChangeName(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		updated, err := q.Get(job.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if updated.Name != "NewName" {
			t.Errorf("Name = %q; want NewName", updated.Name)
		}
	})

	t.Run("missing value2 is a 400", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})
		req := httptest.NewRequest(http.MethodGet, "/?value="+job.ID, nil)
		rr := httptest.NewRecorder()
		s.queueChangeName(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d; want 400", rr.Code)
		}
	})
}

// --- queueChangeScript ---

func TestQueueChangeScript_Direct(t *testing.T) {
	t.Parallel()

	t.Run("sets and sanitizes the script", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})

		req := httptest.NewRequest(http.MethodGet,
			"/?value="+job.ID+"&value2="+url.QueryEscape("../evil.sh"), nil)
		rr := httptest.NewRecorder()
		s.queueChangeScript(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		updated, err := q.Get(job.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if updated.Script != "evil.sh" {
			t.Errorf("Script = %q; want sanitized to evil.sh", updated.Script)
		}
	})

	t.Run("unknown job is a 404", func(t *testing.T) {
		t.Parallel()
		s, _ := testQueueServer(t)
		req := httptest.NewRequest(http.MethodGet, "/?value=nonexistent&value2=test.sh", nil)
		rr := httptest.NewRecorder()
		s.queueChangeScript(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Errorf("status = %d; want 404", rr.Code)
		}
	})
}

// --- queueJobDetail ---

func TestQueueJobDetail_Direct(t *testing.T) {
	t.Parallel()

	t.Run("known job returns one populated slot", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		s.queueJobDetail(rr, req, job.ID)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		var resp struct {
			Queue struct {
				NoOfSlots int `json:"noofslots"`
				Slots     []struct {
					NzoID string `json:"nzo_id"`
					Files []any  `json:"files"`
				} `json:"slots"`
			} `json:"queue"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Queue.NoOfSlots != 1 || len(resp.Queue.Slots) != 1 {
			t.Fatalf("slots = %+v; want exactly 1", resp.Queue.Slots)
		}
		if resp.Queue.Slots[0].NzoID != job.ID {
			t.Errorf("nzo_id = %q; want %q", resp.Queue.Slots[0].NzoID, job.ID)
		}
	})

	t.Run("unknown job returns zero slots, still 200", func(t *testing.T) {
		t.Parallel()
		s, _ := testQueueServer(t)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		s.queueJobDetail(rr, req, "does-not-exist")

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rr.Code)
		}
		var resp struct {
			Queue struct {
				NoOfSlots int `json:"noofslots"`
			} `json:"queue"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Queue.NoOfSlots != 0 {
			t.Errorf("noofslots = %d; want 0", resp.Queue.NoOfSlots)
		}
	})
}

// --- modeAddFile ---

func TestModeAddFile_Direct(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("nzbfile", "direct.nzb")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(makeTestNZB(t)); err != nil {
		t.Fatalf("write nzb: %v", err)
	}
	mw.Close() //nolint:errcheck // buffer-backed writer

	req := httptest.NewRequest(http.MethodPost, "/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	s.modeAddFile(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if q.Len() != 1 {
		t.Errorf("queue len = %d; want 1", q.Len())
	}
}

// --- modeAddURL ---

func TestModeAddURL_Direct(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		_, _ = w.Write(makeTestNZB(t))
	}))
	defer ts.Close()

	s, _ := testQueueServer(t)
	s.grabber = urlgrabber.New(urlgrabber.Config{AllowPrivateIPs: true}, mockGrabberHandler{id: "nzo_direct"})

	req := httptest.NewRequest(http.MethodGet, "/?name="+url.QueryEscape(ts.URL+"/good.nzb"), nil)
	rr := httptest.NewRecorder()
	s.modeAddURL(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Status bool     `json:"status"`
		NzoIDs []string `json:"nzo_ids"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.NzoIDs) != 1 || resp.NzoIDs[0] != "nzo_direct" {
		t.Errorf("nzo_ids = %v; want [nzo_direct]", resp.NzoIDs)
	}
}

// --- enqueueNZBData ---

func TestEnqueueNZBData_Direct(t *testing.T) {
	t.Parallel()

	t.Run("valid NZB is parsed and enqueued", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		s.enqueueNZBData(rr, req, makeTestNZB(t), "direct.nzb")

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		if q.Len() != 1 {
			t.Errorf("queue len = %d; want 1", q.Len())
		}
	})

	t.Run("unparsable NZB is a 400 and nothing is enqueued", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		s.enqueueNZBData(rr, req, []byte("not an nzb"), "bad.nzb")

		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d; want 400", rr.Code)
		}
		if q.Len() != 0 {
			t.Errorf("queue len = %d; want 0", q.Len())
		}
	})
}
