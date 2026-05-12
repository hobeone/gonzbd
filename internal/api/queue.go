package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/types"
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
		if s.app != nil {
			s.app.PauseDownloads()
		}
		s.log.Info("downloads paused")
		respondStatus(w)
	case "resume_all":
		s.queue.ResumeAll()
		if s.app != nil {
			s.app.ResumeDownloads()
		}
		s.log.Info("downloads resumed")
		respondStatus(w)
	case "priority":
		s.queuePriority(w, r)
	// Stubbed: no backing implementation yet.
	case "rename", "sort", "delete_nzf", "change_complete_action",
		"change_name", "change_cat", "change_script":
		s.respondError(w, http.StatusBadRequest, "not implemented in this build: "+action)
	case "change_opts":
		s.queueChangeOpts(w, r)
	default:
		s.respondError(w, http.StatusBadRequest, "unknown queue action: "+action)
	}
}

// queueSlot is the per-job JSON shape clients expect in the queue listing.
// Field names must match the Python build_queue response exactly so that
// existing third-party clients (Sonarr, Radarr, etc.) parse them correctly.
type queueSlot struct {
	NzoID          string `json:"nzo_id"`
	Filename       string `json:"filename"`
	Name           string `json:"name"`
	Category       string `json:"cat"`
	Index          int    `json:"index"`
	Priority       string `json:"priority"`
	Status         string `json:"status"`
	Script         string `json:"script"`
	Password       string `json:"password"`
	Size           string `json:"size"`
	SizeLeft       string `json:"sizeleft"`
	MB             string `json:"mb"`
	MBLeft         string `json:"mbleft"`
	Bytes          int64  `json:"bytes"`
	RemainingBytes int64  `json:"remaining_bytes"`
	Percentage     int    `json:"percentage"`
	Timeleft       string `json:"timeleft"`
	ETA            string `json:"eta"`
	PP             string `json:"pp"`
	Warning        string `json:"warning,omitempty"`
	FailedBytes    int64  `json:"failed_bytes"`
	Par2Bytes      int64  `json:"par2_bytes"`
	Par2Files      int    `json:"par2_files"`

	// CurrentStage is a lowercase machine-readable stage identifier
	// derived from Status (download, repair, unpack, sort, move, ...).
	// Distinct from Status (which is the human-readable label) so the UI
	// can switch on stage without case- or text-fragility.
	CurrentStage string `json:"current_stage"`

	// ArticlesRemaining is the count of articles not yet completed.
	// Reflects job.PendingArticles, which is updated on every state
	// mutation (downloaded, failed, retried).
	ArticlesRemaining int `json:"articles_remaining"`

	// ETASeconds is RemainingBytes divided by current aggregate speed.
	// Zero when paused, idle, or speed is below a noise floor.
	ETASeconds int `json:"eta_seconds"`

	// CurrentFile is the subject of the first incomplete file in the
	// job — best-effort indicator of which file is actively being
	// assembled. Empty when the job has no incomplete files (e.g. in
	// post-processing) or when the subject is unparseable.
	CurrentFile string `json:"current_file"`

	// Files is the per-file breakdown for the row's expansion drawer.
	// Only populated when the caller requests it via files=1; otherwise
	// nil and omitted from JSON to keep default queue payloads small.
	Files []queueFile `json:"files,omitempty"`
}

// queueFile is the per-file shape returned in the expansion-drawer
// detail response. Subjects are pre-sanitized (stripped of yEnc tags
// etc.) so the UI can render them directly.
type queueFile struct {
	Name            string `json:"name"`
	Bytes           int64  `json:"bytes"`
	BytesDownloaded int64  `json:"bytes_downloaded"`
	// State is one of "queued", "downloading", "done", "failed".
	State string `json:"state"`
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
	Speed          string      `json:"speed"`
	KBPerSec       string      `json:"kbpersec"`
	MB             string      `json:"mb"`
	MBLeft         string      `json:"mbleft"`
	Size           string      `json:"size"`
	SizeLeft       string      `json:"sizeleft"`
	Timeleft       string      `json:"timeleft"`
	NoOfSlots      int         `json:"noofslots"`
	NoOfSlotsTotal int         `json:"noofslots_total"`
	Limit          int         `json:"limit"`
	Start          int         `json:"start"`
	Slots          []queueSlot `json:"slots"`
}

// stageFromStatus maps the human-readable Status string to a lowercase
// machine-readable stage identifier for the UI to switch on. Unknown
// statuses fall through unchanged (lowercased).
func stageFromStatus(status constants.Status) string {
	switch status {
	case constants.StatusDownloading, constants.StatusFetching, constants.StatusGrabbing:
		return "download"
	case constants.StatusVerifying, constants.StatusRepairing, constants.StatusChecking, constants.StatusQuickCheck:
		return "repair"
	case constants.StatusExtracting:
		return "unpack"
	case constants.StatusMoving:
		return "move"
	case constants.StatusRunning:
		return "script"
	case constants.StatusPaused:
		return "paused"
	case constants.StatusQueued, constants.StatusIdle:
		return "queued"
	case constants.StatusPropagating:
		return "propagating"
	case constants.StatusCompleted:
		return "completed"
	case constants.StatusFailed:
		return "failed"
	case constants.StatusDeleted:
		return "deleted"
	}
	return strings.ToLower(string(status))
}

// firstIncompleteFile returns the subject of the first not-yet-complete
// file in the job, or empty if every file is complete.
func firstIncompleteFile(j *queue.Job) string {
	for i := range j.Files {
		if !j.Files[i].Complete {
			return j.Files[i].Subject
		}
	}
	return ""
}

// fileState classifies a JobFile into a coarse UI state. "downloading"
// fires once any article in the file has completed; before that the
// file is "queued". "failed" wins over "done" when any article failed.
func fileState(f *queue.JobFile) string {
	if f.Complete {
		anyFailed := false
		for ai := range f.Articles {
			if f.Articles[ai].Failed {
				anyFailed = true
				break
			}
		}
		if anyFailed {
			return "failed"
		}
		return "done"
	}
	// Not Complete: any successful article downloads makes us
	// "downloading"; otherwise still "queued".
	if f.BytesDownloaded > 0 {
		return "downloading"
	}
	return "queued"
}

// buildQueueFiles converts a Job's file slice into the API per-file
// shape for the expansion drawer.
func buildQueueFiles(j *queue.Job) []queueFile {
	out := make([]queueFile, 0, len(j.Files))
	for fi := range j.Files {
		f := &j.Files[fi]
		out = append(out, queueFile{
			Name:            f.Subject,
			Bytes:           f.Bytes,
			BytesDownloaded: f.BytesDownloaded,
			State:           fileState(f),
		})
	}
	return out
}

// noiseFloorBPS is the speed below which ETA computation is suppressed
// (returns 0). Random fluctuations in BPS would otherwise produce wildly
// varying ETAs (e.g. 100 hours when the meter dips for a moment).
const noiseFloorBPS = 1024.0 // 1 KiB/s

// buildSlot renders one Job into the API queueSlot shape. paused is the
// queue-wide pause flag; speed is the snapshot aggregate BPS used for
// ETA. index is the slot's display index in the listing (0 for the
// detail endpoint).
func buildSlot(j *queue.Job, paused bool, speed float64, index int) queueSlot {
	var pct int
	if j.TotalBytes > 0 {
		pct = int(100 * (j.TotalBytes - j.RemainingBytes) / j.TotalBytes)
	}

	displayStatus := j.Status
	if paused && j.Status == constants.StatusDownloading {
		displayStatus = constants.StatusPaused
	}

	var etaSeconds int
	timeleft := "0:00:00"
	etaStr := "unknown"
	if !paused && j.Status == constants.StatusDownloading &&
		speed > noiseFloorBPS && j.RemainingBytes > 0 {
		etaSeconds = int(float64(j.RemainingBytes) / speed)
		timeleft = formatDuration(etaSeconds)
		etaStr = timeleft
	}

	return queueSlot{
		NzoID:             j.ID,
		Filename:          j.Filename,
		Name:              j.Name,
		Category:          j.Category,
		Index:             index,
		Priority:          j.Priority.String(),
		Status:            string(displayStatus),
		Script:            nonEmpty(j.Script, "none"),
		Password:          j.Password,
		Size:              formatBytes(j.TotalBytes),
		SizeLeft:          formatBytes(j.RemainingBytes),
		MB:                toMBString(j.TotalBytes),
		MBLeft:            toMBString(j.RemainingBytes),
		Bytes:             j.TotalBytes,
		RemainingBytes:    j.RemainingBytes,
		Percentage:        pct,
		Timeleft:          timeleft,
		ETA:               etaStr,
		PP:                strconv.Itoa(j.PP),
		Warning:           j.Warning,
		FailedBytes:       j.FailedBytes,
		Par2Bytes:         j.Par2Bytes,
		Par2Files:         j.Par2Files,
		CurrentStage:      stageFromStatus(displayStatus),
		ArticlesRemaining: j.PendingArticles,
		ETASeconds:        etaSeconds,
		CurrentFile:       firstIncompleteFile(j),
	}
}

// queueList returns the paginated, filtered queue listing.
//
// When called with nzo_id=<id>&files=1, returns a single-job detail
// response with the same slot fields plus a per-file breakdown for the
// row's expansion drawer.
//
//nolint:gosec // G120: body already limited by loggingMiddleware's MaxBytesReader
func (s *Server) queueList(w http.ResponseWriter, r *http.Request) {
	// Detail fast-path: the UI requests this when a row drawer is
	// open. We deliberately don't include the files array in the
	// default listing — it would balloon payloads for clients that
	// aren't viewing any drawer.
	if nzoID := r.FormValue("nzo_id"); nzoID != "" && r.FormValue("files") == "1" {
		s.queueJobDetail(w, r, nzoID)
		return
	}

	start := intParam(r, "start")
	limit := intParam(r, "limit")
	search := r.FormValue("search")
	catFilter := r.FormValue("cat")
	statusFilter := r.FormValue("status")

	jobs := s.queue.Snapshot()
	paused := s.queue.IsPaused()

	// Snapshot speed once per request so every slot's ETA is computed
	// from the same denominator.
	var speed float64
	if s.app != nil {
		speed = s.app.Speed()
	}

	// Build slots applying filters.
	var slots []queueSlot
	for _, j := range jobs {
		// Post-processing jobs remain in the queue with their current
		// status (Verifying, Repairing, Extracting, etc.) until
		// OnJobDone removes them and moves them to history.
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

		slots = append(slots, buildSlot(j, paused, speed, len(slots)))
	}

	total := len(slots)

	// Ensure slots is never nil so JSON encodes as [] not null.
	if slots == nil {
		slots = []queueSlot{}
	}

	// Paginate.
	if start < 0 {
		start = 0
	}
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
	} else if total > 0 {
		qStatus = "Downloading"
	}

	// Compute queue-level aggregates across ALL filtered slots
	// (not just the paginated page).
	var totalQueueBytes, remainQueueBytes int64
	for _, sl := range slots {
		totalQueueBytes += sl.Bytes
		remainQueueBytes += sl.RemainingBytes
	}

	respondJSON(w, http.StatusOK, queueResponse{
		Status: true,
		Queue: queueDetail{
			Status:         qStatus,
			Paused:         paused,
			Speed:          "0",
			KBPerSec:       "0",
			MB:             toMBString(totalQueueBytes),
			MBLeft:         toMBString(remainQueueBytes),
			Size:           formatBytes(totalQueueBytes),
			SizeLeft:       formatBytes(remainQueueBytes),
			Timeleft:       "0:00:00",
			NoOfSlots:      len(slots),
			NoOfSlotsTotal: total,
			Limit:          limit,
			Start:          start,
			Slots:          slots,
		},
	})
}

// queueJobDetail returns a single-job slot with the per-file breakdown
// populated. Invoked from queueList when nzo_id and files=1 are both
// present. Response shape mirrors the standard queue listing so the
// frontend can reuse the same parser; Slots will contain exactly one
// entry (or zero if the job has been removed).
func (s *Server) queueJobDetail(w http.ResponseWriter, _ *http.Request, nzoID string) {
	job := s.queue.SnapshotJob(nzoID)
	if job == nil {
		respondJSON(w, http.StatusOK, queueResponse{
			Status: true,
			Queue: queueDetail{
				Status:    "Idle",
				Paused:    s.queue.IsPaused(),
				NoOfSlots: 0,
				Slots:     []queueSlot{},
			},
		})
		return
	}

	paused := s.queue.IsPaused()
	var speed float64
	if s.app != nil {
		speed = s.app.Speed()
	}

	slot := buildSlot(job, paused, speed, 0)
	slot.Files = buildQueueFiles(job)

	respondJSON(w, http.StatusOK, queueResponse{
		Status: true,
		Queue: queueDetail{
			Status:         slot.Status,
			Paused:         paused,
			NoOfSlots:      1,
			NoOfSlotsTotal: 1,
			Slots:          []queueSlot{slot},
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
		for _, j := range s.queue.Snapshot() {
			ids = append(ids, j.ID)
		}
	} else {
		ids = splitCSV(value)
	}

	deleteFiles := r.FormValue("delete_files") == "1" || r.FormValue("del_files") == "1"

	var removed []string
	for _, id := range ids {
		if err := s.app.RemoveJob(r.Context(), id, deleteFiles); err == nil {
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
		if job, err := s.queue.Get(id); err == nil {
			s.log.Info("job paused", "job", id, "name", job.Name)
		}
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
		if job, err := s.queue.Get(id); err == nil {
			s.log.Info("job resumed", "job", id, "name", job.Name)
		}
	}
	respondStatus(w)
}

// queuePriority handles name=priority. SABnzbd convention:
// value = nzo_id, value2 = numeric priority.
func (s *Server) queuePriority(w http.ResponseWriter, r *http.Request) {
	nzoID := r.FormValue("value")
	if nzoID == "" {
		s.respondError(w, http.StatusBadRequest, "missing value parameter (nzo_id)")
		return
	}
	priStr := r.FormValue("value2")
	if priStr == "" {
		s.respondError(w, http.StatusBadRequest, "missing value2 parameter (priority)")
		return
	}
	pri := constants.Priority(int8(intParam(r, "value2"))) //nolint:gosec // G115: priority values fit in int8
	if err := s.queue.SetPriority(nzoID, pri); err != nil {
		s.respondError(w, http.StatusNotFound, err.Error())
		return
	}
	s.log.Info("job priority changed", "job", nzoID, "priority", pri.String())
	respondJSON(w, http.StatusOK, map[string]any{
		"status":   true,
		"nzo_ids":  []string{nzoID},
		"position": 0,
	})
}

// queueChangeOpts handles name=change_opts. SABnzbd convention:
// value = nzo_id, value2 = numeric PP level (0–3).
//
//nolint:gosec // G120: body already limited by loggingMiddleware's MaxBytesReader
func (s *Server) queueChangeOpts(w http.ResponseWriter, r *http.Request) {
	nzoID := r.FormValue("value")
	if nzoID == "" {
		s.respondError(w, http.StatusBadRequest, "missing value parameter (nzo_id)")
		return
	}
	ppStr := r.FormValue("value2")
	if ppStr == "" {
		s.respondError(w, http.StatusBadRequest, "missing value2 parameter (pp level)")
		return
	}
	pp := intParam(r, "value2")
	if pp < 0 || pp > 3 {
		s.respondError(w, http.StatusBadRequest, fmt.Sprintf("pp must be 0-3, got %d", pp))
		return
	}
	if err := s.queue.SetPP(nzoID, pp); err != nil {
		s.respondError(w, http.StatusNotFound, err.Error())
		return
	}
	s.log.Info("job PP changed", "job", nzoID, "pp", pp)
	respondJSON(w, http.StatusOK, map[string]any{
		"status":  true,
		"nzo_ids": []string{nzoID},
	})
}

// modeAddFile handles mode=addfile. Accepts multipart NZB uploads.
// Access level: LevelProtected (deliberate deviation from Python's LevelOpen=1;
// upload should require at least NZB-key-level auth in our unified model).
func (s *Server) modeAddFile(w http.ResponseWriter, r *http.Request) {
	if s.queue == nil {
		s.respondError(w, http.StatusInternalServerError, "queue not wired")
		return
	}

	// Body is already size-limited by the middleware's MaxBytesReader.
	// Use a reasonable in-memory limit for multipart parsing; files
	// larger than this are spilled to temp files on disk.
	const maxMultipartMemory = 10 * 1024 * 1024 // 10 MiB
	if err := r.ParseMultipartForm(maxMultipartMemory); err != nil {
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
		PP:       ppParam(r),
		Priority: priorityParam(r),
		Logger:   s.log,
	}

	sOpts := fsutil.SanitizeOptions{}
	if s.config != nil {
		s.config.WithRead(func(cfg *config.Config) {
			sOpts = cfg.Downloads.SanitizeOptions()
			opts.Categories = cfg.Categories
		})
	}
	job, err := queue.NewJob(parsed, opts, sOpts)
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
		NzbName:  r.FormValue("nzbname"),
		PP:       ppParam(r),
		Script:   r.FormValue("script"),
		Priority: priorityParam(r),
	}
	ids, err := s.grabber.Fetch(r.Context(), urlStr, opts)
	if err != nil {
		s.respondError(w, http.StatusBadGateway, "fetch: "+err.Error())
		return
	}
	if ids == nil {
		ids = []string{}
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"status":  true,
		"nzo_ids": ids,
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

	// Reject paths containing ".." to prevent directory traversal.
	// Check the raw input first, since filepath.Clean resolves ".."
	// on absolute paths (e.g., "/foo/../../etc/passwd" → "/etc/passwd"),
	// making a post-Clean check insufficient for catching traversal attempts.
	if strings.Contains(rawPath, "..") {
		s.respondError(w, http.StatusBadRequest, "path must not contain '..'")
		return
	}

	// Clean the path (resolves any '//', trailing slashes).
	clean := filepath.Clean(rawPath)

	// Defense-in-depth: reject paths where ".." survives cleaning.
	if strings.Contains(clean, "..") {
		s.respondError(w, http.StatusBadRequest, "path must not contain '..'")
		return
	}

	f, err := openFile(clean)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, fmt.Sprintf("open %q: %s", clean, err.Error()))
		return
	}
	defer f.Close() //nolint:errcheck // read-only file cleanup

	fi, err := f.Stat()
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "stat file: "+err.Error())
		return
	}
	if fi.Size() > maxUploadBytes {
		s.respondError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("file too large: %d bytes (max %d)", fi.Size(), maxUploadBytes))
		return
	}

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
		PP:       ppParam(r),
		Priority: priorityParam(r),
		Logger:   s.log,
	}

	sOpts := fsutil.SanitizeOptions{}
	if s.config != nil {
		s.config.WithRead(func(cfg *config.Config) {
			sOpts = cfg.Downloads.SanitizeOptions()
			opts.Categories = cfg.Categories
		})
	}
	job, err := queue.NewJob(parsed, opts, sOpts)
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

// toMBString formats bytes as a megabyte string like "1024.00" for SABnzbd API compatibility.
func toMBString(n int64) string {
	return strconv.FormatFloat(float64(n)/float64(1<<20), 'f', 2, 64)
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
// Returns DefaultPriority (inherit from category) when the parameter is absent.
func priorityParam(r *http.Request) constants.Priority {
	s := r.FormValue("priority") //nolint:gosec // G120: body already limited
	if s == "" {
		return constants.DefaultPriority
	}
	return constants.Priority(int8(intParam(r, "priority"))) //nolint:gosec // G115: priority values fit in int8 by design
}

// ppParam extracts the post-processing level from the request.
// Returns types.PPInherit (-1) when absent, meaning "inherit from category".
func ppParam(r *http.Request) int {
	s := r.FormValue("pp") //nolint:gosec // G120: body already limited
	if s == "" {
		return types.PPInherit
	}
	return intParam(r, "pp")
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

// formatDuration renders a non-negative whole-second duration as h:mm:ss
// (matching Python SABnzbd's timeleft format).
func formatDuration(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	h := seconds / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60
	return fmt.Sprintf("%d:%02d:%02d", h, m, s)
}
