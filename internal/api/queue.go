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
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/humanfmt"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
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
	if s.dispatcher != nil {
		s.dispatcher.Pause()
	}
	if s.downloads != nil {
		s.downloads.PauseDownloads()
	}
	s.log.Info("downloads paused")
	respondStatus(w)
}

func (s *Server) queueResumeAll(w http.ResponseWriter, _ *http.Request) {
	if s.dispatcher != nil {
		s.dispatcher.Resume()
	}
	if s.downloads != nil {
		s.downloads.ResumeDownloads()
		s.downloads.ReevaluateStalls()
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
	// RecoveryBytes/RecoveryFiles describe the job's par2 recovery volumes,
	// excluding the always-downloaded par2 index. Not SABnzbd-Python fields —
	// they have no counterpart in build_queue — so unlike the names above they
	// carry no third-party compatibility constraint.
	RecoveryBytes int64 `json:"recovery_bytes"`
	RecoveryFiles int   `json:"recovery_files"`
	// RepairState is the job's repairability verdict, derived server-side by
	// job.RepairStateFrom and shared with the two abort gates.
	//
	// It is sent as a verdict rather than as the figures behind it so that a
	// client cannot reach a conclusion the backend declines to reach. That is
	// not hypothetical: while the UI re-derived the comparison from raw
	// fields, it condemned jobs the downloader was still working on, twice,
	// and no reference search over Go could find it doing so.
	RepairState       job.RepairState      `json:"repair_state"`
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
	//
	// This is also R26's "articles outstanding": an article is resolved only
	// by a barrier's ack or a permanent failure, so what is left here is
	// exactly what a crash would re-fetch, alongside the two byte figures
	// below.
	ArticlesRemaining int `json:"articles_remaining"`

	// ETASeconds is RemainingBytes divided by current aggregate speed.
	// Zero when paused, idle, or speed is below a noise floor.
	ETASeconds int `json:"eta_seconds"`

	// CurrentFile is the subject of the first incomplete file in the
	// job — best-effort indicator of which file is actively being
	// assembled. Empty when the job has no incomplete files (e.g. in
	// post-processing) or when the subject is unparseable.
	CurrentFile string `json:"current_file"`

	// StallReason is why the job is parked on a storage fault, or "" when it
	// is not (R27). Always present rather than omitempty, so a client can
	// tell an unstalled job from an older server that does not send the
	// field at all. It is the difference between a recoverable full disk and
	// a job that appears to have silently stopped.
	//
	// Sourced from the application rather than from Warning, which the queue
	// wipes on the Resume each re-evaluation performs — a user polling during
	// one would watch the reason blink out and come back.
	StallReason string `json:"stall_reason"`

	// BytesDurable is what a completed fsync covers. BytesPending is what has
	// been written since the job's current checkpoint window opened: accepted
	// by the OS, not yet fsynced, and lost on a power failure — the rework
	// window made visible rather than inferred (R26).
	//
	// They are reported separately and MUST NOT be summed by a client, for
	// two independent reasons. They make different claims, and a total would
	// assert the stronger of the two about all of it. And they are not in the
	// same unit.
	//
	// bytes_durable is derived from the job's progress, which counts the
	// NZB-DECLARED size of each resolved article -- yEnc-ENCODED bytes,
	// which run a few percent above what lands on disk. bytes_pending
	// accumulates len(data) per accepted article -- DECODED bytes, the ones
	// actually written. So bytes_durable - bytes_pending is not a quantity,
	// and neither is their sum.
	//
	// Neither figure can be moved to the other's unit without breaking what
	// it exists for. bytes_durable pairs with size/sizeleft/mb, which are the
	// encoded NZB figures a client renders beside it, and summing the
	// durability record's lengths instead -- a decoded figure -- is the exact
	// substitution docs/queue-lifecycle.md records as having overstated every
	// non-resident job's remaining bytes.
	// bytes_pending feeds B1's volume bound, which measures rework at risk
	// and is therefore about bytes on disk by definition.
	//
	// The two are an order of magnitude apart in practice -- a checkpoint
	// window holds megabytes where a job holds gigabytes -- so the encoding
	// overhead is not the dominant term in any comparison a reader would
	// make. It is documented rather than corrected because there is nothing
	// to correct it to.
	BytesDurable int64 `json:"bytes_durable"`
	BytesPending int64 `json:"bytes_pending"`

	// LastBarrierUnix is when this job's last barrier completed without
	// error, or 0 when none has in this process. Only a SUCCESSFUL barrier
	// stamps it, which is what tells a job that is checkpointing normally
	// from one whose barriers have been failing since a mount went away.
	LastBarrierUnix int64 `json:"last_barrier_unix"`

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
	// State is one of "queued", "downloading", "done", "failed", "held" or
	// "skipped". The last two are par2 recovery volumes the job is not
	// fetching — "held" pending a repair verdict, "skipped" once ruled
	// unnecessary — and are the reason the drawer's file sizes can total more
	// than the row's size, which excludes them.
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

// firstIncompleteFile returns the subject of the first file that has not been
// completely downloaded, skipping files with non-FetchAlways policy.
func firstIncompleteFile(j *job.Job) string {
	m, err := j.Manifest()
	p := j.Progress()
	if err != nil || p == nil {
		return ""
	}
	for i := range m.NumFiles() {
		if p.FileFetchPolicy(i) != job.FetchAlways {
			continue
		}
		if !p.FileComplete(i) {
			return m.FileSubject(i)
		}
	}
	return ""
}

// fileState classifies a file into a coarse UI state. "downloading"
// fires once any article in the file has completed; before that the
// file is "queued". "failed" wins over "done" when any article failed.
func fileState(m *job.Manifest, p *job.JobProgress, fileIdx int) string {
	switch p.FileFetchPolicy(fileIdx) {
	case job.FetchIfNeeded:
		return "held"
	case job.FetchNever:
		return "skipped"
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
//
// Known inconsistency: this lists every manifest file at full size,
// including par2 recovery volumes that are held or discarded, while
// buildSlot's Size/Bytes excludes them (see ExpectedBytes). A drawer's
// per-file total can therefore exceed the row above it for an
// on-demand-par2 job. It is also permanent now rather than transient: a
// discard used to rebuild the manifest without the volumes, so the two
// totals reconverged once the verdict landed, and it now leaves the file in
// place as FetchNever.
//
// Left as-is rather than filtering here: a held volume is still a real file
// the job may yet need, and hiding it from the drawer would make repair
// decisions (which volumes exist, which are held back) invisible where a
// user would look for them.
//
// Reconciling the two views is a UI-side arithmetic question, not a missing
// field — State carries "held" and "skipped" per file, so summing only the
// files in neither state reproduces the row total exactly. Tracked as #325.
func buildQueueFiles(j *job.Job) []queueFile {
	m, err := j.Manifest()
	p := j.Progress()
	if err != nil || p == nil {
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

// buildSlot renders one Row into the API queueSlot shape. paused is the
// queue-wide pause flag; speed is the snapshot aggregate BPS used for
// ETA. index is the slot's display index in the listing (0 for the
// detail endpoint). cp carries the durability figures that live in the
// application rather than in the queue, snapshotted once per request.
func buildSlot(r dispatch.Row, j *job.Job, paused bool, speed float64, index int, duStatus *directunpack.Status, cp app.JobCheckpointState) queueSlot {
	var p *job.JobProgress
	if j != nil {
		p = j.Progress()
	}

	totalBytes := r.Header.Bytes
	remainingBytes := r.Header.Bytes
	if p != nil {
		totalBytes = p.ExpectedBytes()
		remainingBytes = p.RemainingBytes()
	}

	var pct int
	if totalBytes > 0 {
		pct = int(100 * (totalBytes - remainingBytes) / totalBytes)
	}

	displayStatus := r.Status()

	var etaSeconds int
	timeleft := "0:00:00"
	etaStr := "unknown"
	if !paused && displayStatus == constants.StatusDownloading &&
		speed > noiseFloorBPS && remainingBytes > 0 {
		etaSeconds = int(float64(remainingBytes) / speed)
		timeleft = formatDuration(etaSeconds)
		etaStr = timeleft
	}

	var failedBytes int64
	var repairState job.RepairState
	var recoveryBytes int64
	var recoveryFiles int
	var pendingArticles int
	var currentFile string
	var par2Held bool
	var par2ReleaseReason string
	var durableBytes int64

	if j != nil {
		repairState = j.RepairState()
		recoveryBytes = j.RecoveryBytes()
		recoveryFiles = j.RecoveryFiles()
		currentFile = firstIncompleteFile(j)
		par2Held = j.UsesOnDemandPar2()
	}
	if p != nil {
		failedBytes = p.FailedBytes()
		pendingArticles = p.PendingArticles()
		par2ReleaseReason = p.Par2ReleaseReason()
		durableBytes = app.DurableBytesOf(p)
	}

	return queueSlot{
		NzoID:             r.ID,
		Filename:          r.Header.Filename,
		Name:              r.Header.Name,
		Category:          r.Header.Category,
		Index:             index,
		Priority:          constants.Priority(int8(r.Header.Priority)).String(), //nolint:gosec // G115: priority values fit in int8
		Status:            string(displayStatus),
		Script:            nonEmpty(r.Header.Script, "none"),
		Password:          r.Header.Password,
		Size:              humanfmt.Bytes(totalBytes),
		SizeLeft:          humanfmt.Bytes(remainingBytes),
		MB:                toMBString(totalBytes),
		MBLeft:            toMBString(remainingBytes),
		Bytes:             totalBytes,
		RemainingBytes:    remainingBytes,
		Percentage:        pct,
		Timeleft:          timeleft,
		ETA:               etaStr,
		PP:                strconv.Itoa(r.Header.PP),
		Warning:           r.Header.Warning,
		FailedBytes:       failedBytes,
		RepairState:       repairState,
		RecoveryBytes:     recoveryBytes,
		RecoveryFiles:     recoveryFiles,
		CurrentStage:      stageFromStatus(displayStatus),
		ArticlesRemaining: pendingArticles,
		ETASeconds:        etaSeconds,
		CurrentFile:       currentFile,
		Par2Held:          par2Held,
		Par2ReleaseReason: par2ReleaseReason,
		DirectUnpack:      duStatus,
		StallReason:       cp.StallReason,
		BytesDurable:      durableBytes,
		BytesPending:      cp.PendingBytes,
		LastBarrierUnix:   unixOrZero(cp.LastBarrier),
	}
}

// unixOrZero renders a timestamp for JSON, mapping "never" to 0 rather than to
// time.Time's zero unix value of -6795364578871.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// filterQueueSlots applies the category/status/search filters to rows and
// builds the resulting queueSlot list.
func (s *Server) filterQueueSlots(rows []dispatch.Row, catFilter, statusFilter, searchLower string, paused bool, speed float64, duStatuses map[string]directunpack.Status, cpStates map[string]app.JobCheckpointState) []queueSlot {
	slots := make([]queueSlot, 0, len(rows))
	for _, r := range rows {
		st := string(r.Status())
		if catFilter != "" && r.Header.Category != catFilter {
			continue
		}
		if statusFilter != "" && st != statusFilter {
			continue
		}
		if searchLower != "" && !strings.Contains(strings.ToLower(r.Header.Name), searchLower) &&
			!strings.Contains(strings.ToLower(r.Header.Filename), searchLower) {
			continue
		}

		var duStatus *directunpack.Status
		if status, ok := duStatuses[r.ID]; ok {
			duStatus = &status
		}
		var j *job.Job
		if s.dispatcher != nil {
			j, _ = s.dispatcher.Job(r.ID)
		}
		slots = append(slots, buildSlot(r, j, paused, speed, len(slots), duStatus, cpStates[r.ID]))
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
	var cpStates map[string]app.JobCheckpointState
	if s.status != nil {
		duStatuses = s.status.DirectUnpackStatuses()
		cpStates = s.status.CheckpointStates()
	}

	var slots []queueSlot
	var paused bool
	if s.dispatcher != nil {
		rows := s.dispatcher.List()
		paused = s.dispatcher.Paused()
		slots = s.filterQueueSlots(rows, catFilter, statusFilter, searchLower, paused, speed, duStatuses, cpStates)
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
	if s.dispatcher == nil {
		return
	}
	row, ok := s.dispatcher.Row(nzoID)
	if !ok {
		respondJSON(w, http.StatusOK, queueResponse{
			Status: true,
			Queue: queueDetail{
				Status:    "Idle",
				Paused:    s.dispatcher.Paused(),
				NoOfSlots: 0,
				Slots:     []queueSlot{},
			},
		})
		return
	}

	paused := s.dispatcher.Paused()
	var speed float64
	if s.status != nil {
		speed = s.status.Speed()
	}

	var duStatus *directunpack.Status
	var cp app.JobCheckpointState
	if s.status != nil {
		if status, ok := s.status.DirectUnpackStatus(nzoID); ok {
			duStatus = &status
		}
		cp = s.status.CheckpointState(nzoID)
	}
	j, _ := s.dispatcher.Job(nzoID)
	slot := buildSlot(row, j, paused, speed, 0, duStatus, cp)
	if j != nil {
		slot.Files = buildQueueFiles(j)
	}

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
		if s.dispatcher != nil {
			for _, row := range s.dispatcher.List() {
				ids = append(ids, row.ID)
			}
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
	s.queueSetPaused(w, r, "paused")
}

// queueResumeJobs resumes specific jobs by ID (CSV in value=).
func (s *Server) queueResumeJobs(w http.ResponseWriter, r *http.Request) {
	s.queueSetPaused(w, r, "resumed")
	// R19's "on user action". A user who has just cleared a full disk and
	// pressed resume should not also wait out the re-evaluation interval —
	// and for a job parked by a failed file finalize, Queue.Resume alone does
	// not finish the completion the stall interrupted.
	if s.downloads != nil {
		s.downloads.ReevaluateStalls()
	}
}

// queueSetPaused applies action (Pause or Resume) to each job ID in the
// CSV value= parameter, logging the result with the given verb. Not-found
// IDs are silently ignored, matching SABnzbd's lenient bulk semantics.
func (s *Server) queueSetPaused(w http.ResponseWriter, r *http.Request, verb string) {
	value, ok := s.requireParam(w, r, "value", "")
	if !ok {
		return
	}
	for _, id := range splitCSV(value) {
		var name string
		if s.dispatcher != nil {
			if verb == "paused" {
				_ = s.dispatcher.PauseJob(id)
			} else {
				_ = s.dispatcher.ResumeJob(id)
				s.dispatcher.Tick(r.Context())
				s.dispatcher.Tick(r.Context())
			}
			if row, ok := s.dispatcher.Row(id); ok {
				name = row.Header.Name
			}
		}
		if name != "" {
			s.log.Info("job "+verb, "job", id, "name", name)
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
	var err error
	if s.dispatcher != nil {
		err = s.dispatcher.SetPriority(nzoID, int(pri))
	} else {
		s.respondError(w, http.StatusInternalServerError, "dispatcher not wired")
		return
	}
	if err != nil {
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
	var err error
	if s.dispatcher != nil {
		err = s.dispatcher.SetPP(nzoID, pp)
	} else {
		s.respondError(w, http.StatusInternalServerError, "dispatcher not wired")
		return
	}
	if err != nil {
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
	if s.dispatcher != nil {
		resolvedCat := config.FindCategory(cats, cat)
		cat = resolvedCat.Name
		if err := s.dispatcher.SetCategory(nzoID, cat); err != nil {
			s.respondError(w, http.StatusNotFound, err.Error())
			return
		}
		_ = s.dispatcher.SetPP(nzoID, resolvedCat.PP)
		_ = s.dispatcher.SetScript(nzoID, resolvedCat.Script)
		_ = s.dispatcher.SetPriority(nzoID, resolvedCat.Priority)
		row, _ := s.dispatcher.Row(nzoID)
		s.log.Info("job category changed", "job", nzoID,
			"cat", row.Header.Category, "pp", row.Header.PP, "script", row.Header.Script, "priority", row.Header.Priority)
		respondJSON(w, http.StatusOK, map[string]any{
			"status":  true,
			"nzo_ids": []string{nzoID},
		})
		return
	}
	s.respondError(w, http.StatusInternalServerError, "dispatcher not wired")
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
	var err error
	if s.dispatcher != nil {
		err = s.dispatcher.SetName(nzoID, name)
	} else {
		s.respondError(w, http.StatusInternalServerError, "dispatcher not wired")
		return
	}
	if err != nil {
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
	var err error
	if s.dispatcher != nil {
		err = s.dispatcher.SetScript(nzoID, script)
	} else {
		s.respondError(w, http.StatusInternalServerError, "dispatcher not wired")
		return
	}
	if err != nil {
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
	j, hdr, err := app.BuildIngestJob(s.config, parsed, filename, opts, s.log)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.jobs.AddJob(r.Context(), j, hdr, data, false); err != nil {
		s.respondError(w, http.StatusInternalServerError, "enqueue: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"status":  true,
		"nzo_ids": []string{j.ID()},
	})
}
