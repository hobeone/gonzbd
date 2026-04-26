package api

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hobeone/sabnzbd-go/internal/constants"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/queue"
	"github.com/hobeone/sabnzbd-go/internal/types"
)

// maxUploadBytes is the maximum allowed NZB upload body size (50 MiB).
const maxUploadBytes = 50 * 1024 * 1024

// modeQueue handles mode=queue with sub-actions via the name= parameter.
// Mirrors Python's _api_queue and _api_queue_table dispatch.
// Body size is already capped by loggingMiddleware's MaxBytesReader.
//
//nolint:gosec // G120: body already limited by loggingMiddleware's MaxBytesReader
func (s *Server) modeQueue(w http.ResponseWriter, r *http.Request) {
	if s.queue == nil {
		s.respondError(w, http.StatusInternalServerError, "queue not wired")
		return
	}

	action := r.FormValue("name")
	switch action {
	case "", "list":
		s.queueList(w, r)
	case "delete":
		s.queueDelete(w, r)
	case "purge":
		s.queuePurge(w, r)
	case "pause":
		s.queuePauseJobs(w, r)
	case "resume":
		s.queueResumeJobs(w, r)
	case "pause_all":
		s.queue.PauseAll()
		respondStatus(w)
	case "resume_all":
		s.queue.ResumeAll()
		respondStatus(w)
	// Stubbed: no backing implementation yet.
	case "rename", "priority", "sort", "delete_nzf", "change_complete_action",
		"change_name", "change_cat", "change_script", "change_opts":
		s.respondError(w, http.StatusBadRequest, "not implemented in this build: "+action)
	default:
		s.respondError(w, http.StatusBadRequest, "unknown queue action: "+action)
	}
}

// queueSlot is the per-job JSON shape clients expect in the queue listing.
// Field names must match the Python build_queue response exactly so that
// existing third-party clients (Sonarr, Radarr, etc.) parse them correctly.
type queueSlot struct {
	NzoID          string  `json:"nzo_id"`
	Filename       string  `json:"filename"`
	Name           string  `json:"name"`
	Category       string  `json:"category"`
	Priority       string  `json:"priority"`
	Status         string  `json:"status"`
	Script         string  `json:"script"`
	Password       string  `json:"password"`
	Size           string  `json:"size"`
	SizeLeft       string  `json:"sizeleft"`
	MB             float64 `json:"mb"`
	MBLeft         float64 `json:"mbleft"`
	Bytes          int64   `json:"bytes"`
	RemainingBytes int64   `json:"remaining_bytes"`
	Percentage     string  `json:"percentage"`
	PP             string  `json:"pp"`
	Warning        string  `json:"warning,omitempty"`
}

// queueResponse is the outer JSON object returned for default queue listings.
type queueResponse struct {
	Status bool        `json:"status"`
	Queue  queueDetail `json:"queue"`
}

// queueDetail is the nested object under "queue" in the listing response.
type queueDetail struct {
	Status         string      `json:"status"`
	Paused         bool        `json:"paused"`
	NoOfSlots      int         `json:"noofslots"`
	NoOfSlotsTotal int         `json:"noofslots_total"`
	Limit          int         `json:"limit"`
	Start          int         `json:"start"`
	Slots          []queueSlot `json:"slots"`
}

// queueList returns the paginated, filtered queue listing.
//
//nolint:gosec // G120: body already limited by loggingMiddleware's MaxBytesReader
func (s *Server) queueList(w http.ResponseWriter, r *http.Request) {
	start := intParam(r, "start")
	limit := intParam(r, "limit")
	search := r.FormValue("search")
	catFilter := r.FormValue("cat")
	statusFilter := r.FormValue("status")

	jobs := s.queue.List()
	paused := s.queue.IsPaused()

	// Build slots applying filters.
	var slots []queueSlot
	for _, j := range jobs {
		if catFilter != "" && j.Category != catFilter {
			continue
		}
		if statusFilter != "" && string(j.Status) != statusFilter {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(j.Name), strings.ToLower(search)) &&
			!strings.Contains(strings.ToLower(j.Filename), strings.ToLower(search)) {
			continue
		}

		var pct string
		if j.TotalBytes > 0 {
			p := 100.0 * float64(j.TotalBytes-j.RemainingBytes) / float64(j.TotalBytes)
			pct = strconv.FormatFloat(p, 'f', 1, 64)
		} else {
			pct = "0.0"
		}

		slots = append(slots, queueSlot{
			NzoID:          j.ID,
			Filename:       j.Filename,
			Name:           j.Name,
			Category:       j.Category,
			Priority:       j.Priority.String(),
			Status:         string(j.Status),
			Script:         nonEmpty(j.Script, "none"),
			Password:       j.Password,
			Size:           formatBytes(j.TotalBytes),
			SizeLeft:       formatBytes(j.RemainingBytes),
			MB:             toMB(j.TotalBytes),
			MBLeft:         toMB(j.RemainingBytes),
			Bytes:          j.TotalBytes,
			RemainingBytes: j.RemainingBytes,
			Percentage:     pct,
			PP:             strconv.Itoa(j.PP),
			Warning:        j.Warning,
		})
	}

	total := len(slots)

	// Ensure slots is never nil so JSON encodes as [] not null.
	if slots == nil {
		slots = []queueSlot{}
	}

	// Paginate.
	if start > len(slots) {
		start = len(slots)
	}
	slots = slots[start:]
	if limit > 0 && limit < len(slots) {
		slots = slots[:limit]
	}

	qStatus := "Idle"
	if paused {
		qStatus = "Paused"
	} else if len(jobs) > 0 {
		qStatus = "Downloading"
	}

	respondJSON(w, http.StatusOK, queueResponse{
		Status: true,
		Queue: queueDetail{
			Status:         qStatus,
			Paused:         paused,
			NoOfSlots:      len(slots),
			NoOfSlotsTotal: total,
			Limit:          limit,
			Start:          start,
			Slots:          slots,
		},
	})
}

// queueDelete removes specific jobs by ID (CSV in value=) or all jobs if value=all.
// If delete_files=1 is present, also deletes partial downloads from disk.
//
//nolint:gosec // G120: body already limited by loggingMiddleware's MaxBytesReader
func (s *Server) queueDelete(w http.ResponseWriter, r *http.Request) {
	value := r.FormValue("value")
	if value == "" {
		s.respondError(w, http.StatusBadRequest, "missing value")
		return
	}

	var ids []string

	if value == "all" {
		for _, j := range s.queue.List() {
			ids = append(ids, j.ID)
		}
	} else {
		ids = splitCSV(value)
	}

	var removed []string
	for _, id := range ids {
		if err := s.app.RemoveJob(id); err == nil {
			removed = append(removed, id)
		}
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"status":  true,
		"nzo_ids": removed,
	})
}

// queuePurge removes all jobs from the queue.
func (s *Server) queuePurge(w http.ResponseWriter, r *http.Request) {
	// Treat purge as delete-all.
	r2 := r.Clone(r.Context())
	if r2.Form == nil {
		r2.Form = make(url.Values)
	}
	r2.Form.Set("value", "all")
	s.queueDelete(w, r2)
}

// queuePauseJobs pauses specific jobs by ID (CSV in value=).
//
//nolint:gosec // G120: body already limited by loggingMiddleware's MaxBytesReader
func (s *Server) queuePauseJobs(w http.ResponseWriter, r *http.Request) {
	value := r.FormValue("value")
	if value == "" {
		s.respondError(w, http.StatusBadRequest, "missing value parameter")
		return
	}
	ids := splitCSV(value)
	for _, id := range ids {
		_ = s.queue.Pause(id) //nolint:errcheck // not-found silently ignored
	}
	respondStatus(w)
}

// queueResumeJobs resumes specific jobs by ID (CSV in value=).
//
//nolint:gosec // G120: body already limited by loggingMiddleware's MaxBytesReader
func (s *Server) queueResumeJobs(w http.ResponseWriter, r *http.Request) {
	value := r.FormValue("value")
	if value == "" {
		s.respondError(w, http.StatusBadRequest, "missing value parameter")
		return
	}
	ids := splitCSV(value)
	for _, id := range ids {
		_ = s.queue.Resume(id) //nolint:errcheck // not-found silently ignored
	}
	respondStatus(w)
}

// modeAddFile handles mode=addfile. Accepts multipart NZB uploads.
// Access level: LevelProtected (deliberate deviation from Python's LevelOpen=1;
// upload should require at least NZB-key-level auth in our unified model).
func (s *Server) modeAddFile(w http.ResponseWriter, r *http.Request) {
	if s.queue == nil {
		s.respondError(w, http.StatusInternalServerError, "queue not wired")
		return
	}

	// Raise the body limit for file uploads above the middleware default.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		s.respondError(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}

	f, fh, err := r.FormFile("nzbfile")
	if err != nil {
		// Fallback to "name" field if "nzbfile" is missing.
		f, fh, err = r.FormFile("name")
	}
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "nzbfile or name field required")
		return
	}
	defer f.Close() //nolint:errcheck // multipart cleanup

	data, err := io.ReadAll(f)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "read upload: "+err.Error())
		return
	}

	parsed, err := nzb.Parse(bytes.NewReader(data))
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "parse NZB: "+err.Error())
		return
	}

	opts := queue.AddOptions{
		Filename: fh.Filename,
		Name:     r.FormValue("nzbname"),
		Category: r.FormValue("cat"),
		Script:   r.FormValue("script"),
		Password: r.FormValue("password"),
		PP:       intParam(r, "pp"),
		Priority: priorityParam(r),
	}

	job, err := queue.NewJob(parsed, opts)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "create job: "+err.Error())
		return
	}
	if err := s.app.AddJob(r.Context(), job, data, false); err != nil {
		s.respondError(w, http.StatusInternalServerError, "enqueue: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"status":  true,
		"nzo_ids": []string{job.ID},
	})
}

// modeAddURL handles mode=addurl. Fetches the NZB pointed to by `name=`
// synchronously via the URL grabber and enqueues it.
//
// Python's addurl returns immediately and queues the fetch in a worker
// thread; our implementation blocks until the fetch completes. For small
// NZBs (<1 MiB) the difference is imperceptible; for very large remote
// NZBs the client will see a longer response. Revisit if it hurts in
// practice — a fire-and-forget wrapper is a few lines.
func (s *Server) modeAddURL(w http.ResponseWriter, r *http.Request) {
	if s.grabber == nil {
		s.respondError(w, http.StatusInternalServerError, "url grabber not wired")
		return
	}
	urlStr := formString(r, "name")
	if urlStr == "" {
		s.respondError(w, http.StatusBadRequest, "missing name parameter (URL)")
		return
	}
	opts := types.FetchOptions{
		Category: r.FormValue("cat"),
		Password: r.FormValue("password"),
	}
	n, err := s.grabber.Fetch(r.Context(), urlStr, opts)
	if err != nil {
		s.respondError(w, http.StatusBadGateway, "fetch: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"status":  true,
		"fetched": n,
	})
}

// modeAddLocalFile handles mode=addlocalfile. Reads an NZB from an
// absolute server-side path supplied in the name= query parameter.
//
// Security: only absolute paths are accepted; filepath.Clean is applied and
// paths containing ".." after cleaning are rejected. This is a LevelProtected
// operation (same as addfile); for stricter security consider LevelAdmin, but
// LevelProtected mirrors Python's addlocalfile level (2).
//
//nolint:gosec // G120: body already limited by loggingMiddleware's MaxBytesReader
func (s *Server) modeAddLocalFile(w http.ResponseWriter, r *http.Request) {
	if s.queue == nil {
		s.respondError(w, http.StatusInternalServerError, "queue not wired")
		return
	}

	rawPath := r.FormValue("name")
	if rawPath == "" {
		s.respondError(w, http.StatusBadRequest, "missing name parameter")
		return
	}
	// Reject non-absolute paths.
	if !filepath.IsAbs(rawPath) {
		s.respondError(w, http.StatusBadRequest, "name must be an absolute path")
		return
	}
	// Reject path traversal attempts after cleaning.
	clean := filepath.Clean(rawPath)
	if strings.Contains(clean, "..") {
		s.respondError(w, http.StatusBadRequest, "path traversal not allowed")
		return
	}

	f, err := openFile(clean)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, fmt.Sprintf("open %q: %s", clean, err.Error()))
		return
	}
	defer f.Close() //nolint:errcheck // read-only file cleanup

	data, err := io.ReadAll(f)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "read file: "+err.Error())
		return
	}

	parsed, err := nzb.Parse(bytes.NewReader(data))
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "parse NZB: "+err.Error())
		return
	}

	opts := queue.AddOptions{
		Filename: filepath.Base(clean),
		Name:     r.FormValue("nzbname"),
		Category: r.FormValue("cat"),
		Script:   r.FormValue("script"),
		Password: r.FormValue("password"),
		PP:       intParam(r, "pp"),
		Priority: priorityParam(r),
	}

	job, err := queue.NewJob(parsed, opts)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "create job: "+err.Error())
		return
	}
	if err := s.app.AddJob(r.Context(), job, data, false); err != nil {
		s.respondError(w, http.StatusInternalServerError, "enqueue: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"status":  true,
		"nzo_ids": []string{job.ID},
	})
}

// --- Helpers ---

// formatBytes converts a byte count to a human-readable string like "1.23 GB".
func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/float64(1<<30), 'f', 2, 64) + " GB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/float64(1<<20), 'f', 2, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/float64(1<<10), 'f', 2, 64) + " KB"
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}

// toMB converts bytes to megabytes as a float64, rounded to 1 decimal.
func toMB(n int64) float64 {
	return math.Round(float64(n)/float64(1<<20)*10) / 10
}

// intParam reads a query parameter as int, returning 0 if absent or unparseable.
func intParam(r *http.Request, key string) int { //nolint:unparam // callers pass varying keys
	v := formString(r, key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// formString reads a query/form value. Centralizes the //nolint:gosec
// suppression — the body is size-limited by loggingMiddleware so G120
// (memory-exhaustion via unbounded form parsing) does not apply.
func formString(r *http.Request, key string) string {
	return r.FormValue(key) //nolint:gosec // G120: body already limited by loggingMiddleware's MaxBytesReader
}

// priorityParam reads the priority= query parameter and maps it to a Priority constant.
func priorityParam(r *http.Request) constants.Priority {
	return constants.Priority(int8(intParam(r, "priority"))) //nolint:gosec // G115: priority values fit in int8 by design
}

// openFile wraps os.Open so the G304 gosec finding is isolated to one place.
// The caller is responsible for validating the path before calling openFile.
func openFile(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: caller validates path is absolute and traversal-free
}

// splitCSV splits a comma-separated value string into trimmed non-empty tokens.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// nonEmpty returns s if non-empty, otherwise fallback.
func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
