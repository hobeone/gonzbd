package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/humanfmt"
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
	if !s.requireQueue(w) {
		return
	}

	action := formValue(r, "name")
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
		s.queuePauseAll(w, r)
	case "resume_all":
		s.queueResumeAll(w, r)
	case "priority":
		s.queuePriority(w, r)
	case "rename", "change_name":
		s.queueChangeName(w, r)
	case "change_script":
		s.queueChangeScript(w, r)
	case "sort", "delete_nzf":
		s.queueNotImplemented(w, action)
	case "change_complete_action":
		s.queueChangeCompleteAction(w, r)
	case "change_opts":
		s.queueChangeOpts(w, r)
	case "change_cat":
		s.queueChangeCat(w, r)
	default:
		s.queueUnknownAction(w, action)
	}
}

func (s *Server) queuePauseAll(w http.ResponseWriter, _ *http.Request) {
	s.queue.PauseAll()
	if s.downloads != nil {
		s.downloads.PauseDownloads()
	}
	s.log.Info("downloads paused")
	respondStatus(w)
}

func (s *Server) queueResumeAll(w http.ResponseWriter, r *http.Request) {
	s.queue.ResumeAll(r.Context())
	if s.downloads != nil {
		s.downloads.ResumeDownloads()
	}
	s.log.Info("downloads resumed")
	respondStatus(w)
}

func (s *Server) queueNotImplemented(w http.ResponseWriter, action string) {
	s.respondError(w, http.StatusBadRequest, "not implemented in this build: "+action)
}

func (s *Server) queueChangeCompleteAction(w http.ResponseWriter, _ *http.Request) {
	respondStatus(w)
}

func (s *Server) queueUnknownAction(w http.ResponseWriter, action string) {
	s.respondError(w, http.StatusBadRequest, "unknown queue action: "+action)
}

// queueSlot is the per-job JSON shape clients expect in the queue listing.
// Field names must match the Python build_queue response exactly so that
// existing third-party clients (Sonarr, Radarr, etc.) parse them correctly.
type queueSlot struct {
	NzoID             string               `json:"nzo_id"`
	Filename          string               `json:"filename"`
	Name              string               `json:"name"`
	Category          string               `json:"cat"`
	Index             int                  `json:"index"`
	Priority          string               `json:"priority"`
	Status            string               `json:"status"`
	Script            string               `json:"script"`
	Password          string               `json:"password"`
	Size              string               `json:"size"`
	SizeLeft          string               `json:"sizeleft"`
	MB                string               `json:"mb"`
	MBLeft            string               `json:"mbleft"`
	Bytes             int64                `json:"bytes"`
	RemainingBytes    int64                `json:"remaining_bytes"`
	Percentage        int                  `json:"percentage"`
	Timeleft          string               `json:"timeleft"`
	ETA               string               `json:"eta"`
	PP                string               `json:"pp"`
	Warning           string               `json:"warning,omitempty"`
	FailedBytes       int64                `json:"failed_bytes"`
	Par2Bytes         int64                `json:"par2_bytes"`
	Par2Files         int                  `json:"par2_files"`
	Par2Held          bool                 `json:"par2_held,omitempty"`
	Par2ReleaseReason string               `json:"par2_release_reason,omitempty"`
	DirectUnpack      *directunpack.Status `json:"direct_unpack,omitempty"`

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
	m, p := j.Manifest(), j.Progress()
	if m == nil || p == nil {
		return ""
	}
	for i := range m.NumFiles() {
		if !p.FileComplete(i) {
			return m.FileSubject(i)
		}
	}
	return ""
}

// fileState classifies a file into a coarse UI state. "downloading"
// fires once any article in the file has completed; before that the
// file is "queued". "failed" wins over "done" when any article failed.
func fileState(m *queue.Manifest, p *queue.JobProgress, fileIdx int) string {
	if p.FileDeferred(fileIdx) {
		return "held"
	}
	if p.FileComplete(fileIdx) {
		anyFailed := false
		lo, hi := m.FileRange(fileIdx)
		for i := lo; i < hi; i++ {
			if p.ArticleFailed(i) {
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
	if p.FileBytesDownloaded(fileIdx) > 0 {
		return "downloading"
	}
	return "queued"
}

// buildQueueFiles converts a Job's file slice into the API per-file
// shape for the expansion drawer.
func buildQueueFiles(j *queue.Job) []queueFile {
	m, p := j.Manifest(), j.Progress()
	if m == nil || p == nil {
		return []queueFile{}
	}
	out := make([]queueFile, 0, m.NumFiles())
	for fi := range m.NumFiles() {
		out = append(out, queueFile{
			Name:            m.FileSubject(fi),
			Bytes:           m.FileBytes(fi),
			BytesDownloaded: p.FileBytesDownloaded(fi),
			State:           fileState(m, p, fi),
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
func buildSlot(j *queue.Job, paused bool, speed float64, index int, duStatus *directunpack.Status) queueSlot {
	// No manifest access: every value below comes from the job's promoted
	// scalars or from JobProgress, both of which are resident for the life
	// of the job. A queue listing is polled continuously and includes every
	// queued and paused job, all of which have had their manifests evicted,
	// so needing one here meant either a disk read per job per poll or a nil
	// deref — this used to do both.
	p := j.Progress()
	totalBytes := j.TotalBytes()
	remainingBytes := p.RemainingBytes()

	var pct int
	if totalBytes > 0 {
		pct = int(100 * (totalBytes - remainingBytes) / totalBytes)
	}

	displayStatus := j.Status
	if paused && j.Status == constants.StatusDownloading {
		displayStatus = constants.StatusPaused
	}

	var etaSeconds int
	timeleft := "0:00:00"
	etaStr := "unknown"
	if !paused && j.Status == constants.StatusDownloading &&
		speed > noiseFloorBPS && remainingBytes > 0 {
		etaSeconds = int(float64(remainingBytes) / speed)
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
		Size:              humanfmt.Bytes(totalBytes),
		SizeLeft:          humanfmt.Bytes(remainingBytes),
		MB:                toMBString(totalBytes),
		MBLeft:            toMBString(remainingBytes),
		Bytes:             totalBytes,
		RemainingBytes:    remainingBytes,
		Percentage:        pct,
		Timeleft:          timeleft,
		ETA:               etaStr,
		PP:                strconv.Itoa(j.PP),
		Warning:           j.Warning,
		FailedBytes:       p.FailedBytes(),
		Par2Bytes:         j.Par2Bytes(),
		Par2Files:         j.Par2Files(),
		CurrentStage:      stageFromStatus(displayStatus),
		ArticlesRemaining: p.PendingArticles(),
		ETASeconds:        etaSeconds,
		CurrentFile:       firstIncompleteFile(j),
		Par2Held:          j.HasDeferredPar2(),
		Par2ReleaseReason: p.Par2ReleaseReason(),
		DirectUnpack:      duStatus,
	}
}

// filterQueueSlots applies the category/status/search filters to jobs and
// builds the resulting queueSlot list. Split out of queueList to isolate
// per-job filtering from pagination and response assembly (OPT-9).
func filterQueueSlots(jobs []*queue.Job, catFilter, statusFilter, searchLower string, paused bool, speed float64, duStatuses map[string]directunpack.Status) []queueSlot {
	slots := make([]queueSlot, 0, len(jobs))
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
		if searchLower != "" && !strings.Contains(strings.ToLower(j.Name), searchLower) &&
			!strings.Contains(strings.ToLower(j.Filename), searchLower) {
			continue
		}

		var duStatus *directunpack.Status
		if status, ok := duStatuses[j.ID]; ok {
			duStatus = &status
		}
		slots = append(slots, buildSlot(j, paused, speed, len(slots), duStatus))
	}
	return slots
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
	if nzoID := formValue(r, "nzo_id"); nzoID != "" && formValue(r, "files") == "1" {
		s.queueJobDetail(w, r, nzoID)
		return
	}

	start := intParam(r, "start")
	limit := intParam(r, "limit")
	search := formValue(r, "search")
	catFilter := formValue(r, "cat")
	statusFilter := formValue(r, "status")

	jobs := s.queue.Snapshot()
	paused := s.queue.IsPaused()

	// Snapshot speed once per request so every slot's ETA is computed
	// from the same denominator.
	var speed float64
	if s.status != nil {
		speed = s.status.Speed()
	}

	// Hoisted out of the loop (OPT-10): lowercase search once per request
	// instead of re-lowercasing it (and each job's Name/Filename) on every
	// iteration.
	searchLower := strings.ToLower(search)

	// Snapshot all direct-unpack statuses once per request (OPT-12) instead
	// of re-locking app.mu (application-wide) once per job in the loop below.
	var duStatuses map[string]directunpack.Status
	if s.status != nil {
		duStatuses = s.status.DirectUnpackStatuses()
	}

	// Build slots applying filters.
	slots := filterQueueSlots(jobs, catFilter, statusFilter, searchLower, paused, speed, duStatuses)

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
			Size:           humanfmt.Bytes(totalQueueBytes),
			SizeLeft:       humanfmt.Bytes(remainQueueBytes),
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
	if s.status != nil {
		speed = s.status.Speed()
	}

	var duStatus *directunpack.Status
	if s.status != nil {
		if status, ok := s.status.DirectUnpackStatus(job.ID); ok {
			duStatus = &status
		}
	}
	slot := buildSlot(job, paused, speed, 0, duStatus)
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
	value, ok := s.requireParam(w, r, "value", "")
	if !ok {
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

	deleteFiles := formValue(r, "delete_files") == "1" || formValue(r, "del_files") == "1"

	var removed []string
	for _, id := range ids {
		if err := s.jobs.RemoveJob(r.Context(), id, deleteFiles); err == nil {
			removed = append(removed, id)
		} else {
			s.log.WarnContext(r.Context(), "failed to remove job during bulk delete", "id", id, "error", err)
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
func (s *Server) queuePauseJobs(w http.ResponseWriter, r *http.Request) {
	s.queueSetPaused(w, r, s.queue.Pause, "paused")
}

// queueResumeJobs resumes specific jobs by ID (CSV in value=).
func (s *Server) queueResumeJobs(w http.ResponseWriter, r *http.Request) {
	s.queueSetPaused(w, r, s.queue.Resume, "resumed")
}

// queueSetPaused applies action (Pause or Resume) to each job ID in the
// CSV value= parameter, logging the result with the given verb. Not-found
// IDs are silently ignored, matching SABnzbd's lenient bulk semantics.
func (s *Server) queueSetPaused(w http.ResponseWriter, r *http.Request, action func(string) error, verb string) {
	value, ok := s.requireParam(w, r, "value", "")
	if !ok {
		return
	}
	for _, id := range splitCSV(value) {
		_ = action(id) //nolint:errcheck // not-found silently ignored
		// Use SnapshotJob (deep copy under RLock), never the live *Job
		// from queue.Get: the download pipeline mutates job.Name
		// concurrently, and reading it off the live pointer here would
		// be an unsynchronized data race (TRACE-1).
		if job := s.queue.SnapshotJob(id); job != nil {
			s.log.Info("job "+verb, "job", id, "name", job.Name)
		}
	}
	respondStatus(w)
}

// queuePriority handles name=priority. SABnzbd convention:
// value = nzo_id, value2 = numeric priority.
func (s *Server) queuePriority(w http.ResponseWriter, r *http.Request) {
	nzoID, ok := s.requireParam(w, r, "value", "nzo_id")
	if !ok {
		return
	}
	if _, ok := s.requireParam(w, r, "value2", "priority"); !ok {
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
func (s *Server) queueChangeOpts(w http.ResponseWriter, r *http.Request) {
	nzoID, ok := s.requireParam(w, r, "value", "nzo_id")
	if !ok {
		return
	}
	if _, ok := s.requireParam(w, r, "value2", "pp level"); !ok {
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

// queueChangeCat handles name=change_cat.
// SABnzbd convention: value = nzo_id, value2 = category name.
// Resolves the category from config and inherits its PP level, script, and
// priority — matching SABnzbd's NzbQueue.change_cat semantics. An empty
// value2 resets the job to the configured Default category.
//
//nolint:gosec // G120: body already limited by loggingMiddleware's MaxBytesReader
func (s *Server) queueChangeCat(w http.ResponseWriter, r *http.Request) {
	nzoID, ok := s.requireParam(w, r, "value", "nzo_id")
	if !ok {
		return
	}
	cat := formValue(r, "value2") // empty string → FindCategory falls back to Default

	var cats []config.CategoryConfig
	if s.config != nil {
		cats = s.config.GetCategories()
	}
	if err := s.queue.SetCategory(nzoID, cat, cats); err != nil {
		s.respondError(w, http.StatusNotFound, err.Error())
		return
	}
	// Use SnapshotJob (deep copy under RLock), never the live *Job from
	// queue.Get: the download pipeline mutates these fields concurrently,
	// and reading them off the live pointer here would be an
	// unsynchronized data race (TRACE-1).
	job := s.queue.SnapshotJob(nzoID)
	s.respondCategoryChanged(w, nzoID, job)
}

// respondCategoryChanged writes the change_cat success response. job is the
// post-change snapshot used only for the log line and may be nil: SetCategory
// has already succeeded by the time this is called, and a concurrent removal
// of the job between that call and the snapshot is not a failure of this
// request — it is only missing logging detail, so the response must stay
// 200 regardless.
func (s *Server) respondCategoryChanged(w http.ResponseWriter, nzoID string, job *queue.Job) {
	if job != nil {
		s.log.Info("job category changed", "job", nzoID,
			"cat", job.Category, "pp", job.PP, "script", job.Script, "priority", job.Priority)
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"status":  true,
		"nzo_ids": []string{nzoID},
	})
}

// queueChangeName handles name=rename and name=change_name.
// SABnzbd convention: value = nzo_id, value2 = new name.
func (s *Server) queueChangeName(w http.ResponseWriter, r *http.Request) {
	nzoID, ok := s.requireParam(w, r, "value", "nzo_id")
	if !ok {
		return
	}
	name, ok := s.requireParam(w, r, "value2", "name")
	if !ok {
		return
	}
	if err := s.queue.SetName(nzoID, name); err != nil {
		s.respondError(w, http.StatusNotFound, err.Error())
		return
	}
	s.log.Info("job renamed", "job", nzoID, "name", name)
	respondJSON(w, http.StatusOK, map[string]any{
		"status":  true,
		"nzo_ids": []string{nzoID},
	})
}

// sanitizeScriptParam cleans a script parameter from API requests.
// It preserves empty string and special values ("None", "Default", case-insensitive)
// while stripping directory components from all other script paths using filepath.Base
// to prevent path traversal and absolute path execution.
func sanitizeScriptParam(script string) string {
	if script == "" {
		return ""
	}
	// Convert Windows backslashes to forward slashes so filepath.Base works across OS boundaries.
	script = strings.ReplaceAll(script, "\\", "/")
	if strings.EqualFold(script, "none") || strings.EqualFold(script, "default") {
		return script
	}
	return filepath.Base(script)
}

// queueChangeScript handles name=change_script.
// SABnzbd convention: value = nzo_id, value2 = script name.
//
//nolint:gosec // G120: body already limited by loggingMiddleware's MaxBytesReader
func (s *Server) queueChangeScript(w http.ResponseWriter, r *http.Request) {
	nzoID, ok := s.requireParam(w, r, "value", "nzo_id")
	if !ok {
		return
	}
	script := sanitizeScriptParam(formValue(r, "value2"))
	if err := s.queue.SetScript(nzoID, script); err != nil {
		s.respondError(w, http.StatusNotFound, err.Error())
		return
	}
	s.log.Info("job script changed", "job", nzoID, "script", script)
	respondJSON(w, http.StatusOK, map[string]any{
		"status":  true,
		"nzo_ids": []string{nzoID},
	})
}

// modeAddFile handles mode=addfile. Accepts multipart NZB uploads.
// Access level: LevelProtected (deliberate deviation from Python's LevelOpen=1;
// upload should require at least NZB-key-level auth in our unified model).
func (s *Server) modeAddFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireQueue(w) {
		return
	}

	// Body is already size-limited by the middleware's MaxBytesReader.
	// Use a reasonable in-memory limit for multipart parsing; files
	// larger than this are spilled to temp files on disk.
	const maxMultipartMemory = 10 * 1024 * 1024                      // 10 MiB
	if err := r.ParseMultipartForm(maxMultipartMemory); err != nil { //nolint:gosec // body size bounded by MaxBytesReader middleware
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

	s.enqueueNZBData(w, r, data, fh.Filename)
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
	if !s.requireGrabber(w) {
		return
	}
	urlStr, ok := s.requireParam(w, r, "name", "URL")
	if !ok {
		return
	}
	opts := types.FetchOptions{
		Category: formValue(r, "cat"),      //nolint:gosec // body size bounded by MaxBytesReader middleware
		Password: formValue(r, "password"), //nolint:gosec // body size bounded by MaxBytesReader middleware
		NzbName:  formValue(r, "nzbname"),  //nolint:gosec // body size bounded by MaxBytesReader middleware
		PP:       ppParam(r),
		Script:   sanitizeScriptParam(formValue(r, "script")), //nolint:gosec // body size bounded by MaxBytesReader middleware
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
// paths containing ".." after cleaning are rejected. Restricted to
// LevelAdmin and to paths within a configured picker root (SEC-6) — the
// upload-only NZB key can no longer probe arbitrary filesystem paths.
//
//nolint:gosec // G120: body already limited by loggingMiddleware's MaxBytesReader
func (s *Server) modeAddLocalFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireQueue(w) {
		return
	}

	rawPath, ok := s.requireParam(w, r, "name", "")
	if !ok {
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

	if !s.pathWithinConfiguredRoots(clean) {
		s.respondError(w, http.StatusForbidden, "path is outside configured directories")
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

	s.enqueueNZBData(w, r, data, filepath.Base(clean))
}

// enqueueNZBData parses raw NZB bytes, creates a job, and enqueues it.
// filename is the display name attached to the job (e.g. the upload filename
// or basename of a local path). It writes a JSON response directly and is
// the shared implementation of modeAddFile and modeAddLocalFile.
func (s *Server) enqueueNZBData(w http.ResponseWriter, r *http.Request, data []byte, filename string) {
	parsed, err := nzb.Parse(bytes.NewReader(data))
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "parse NZB: "+err.Error())
		return
	}

	opts := types.FetchOptions{
		NzbName:  formValue(r, "nzbname"),                     //nolint:gosec // body size bounded by MaxBytesReader middleware
		Category: formValue(r, "cat"),                         //nolint:gosec // body size bounded by MaxBytesReader middleware
		Script:   sanitizeScriptParam(formValue(r, "script")), //nolint:gosec // body size bounded by MaxBytesReader middleware
		Password: formValue(r, "password"),                    //nolint:gosec // body size bounded by MaxBytesReader middleware
		PP:       ppParam(r),
		Priority: priorityParam(r),
	}
	job, err := app.BuildIngestJob(s.config, parsed, filename, opts, s.log)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.jobs.AddJob(r.Context(), job, data, false); err != nil {
		s.respondError(w, http.StatusInternalServerError, "enqueue: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"status":  true,
		"nzo_ids": []string{job.ID},
	})
}
