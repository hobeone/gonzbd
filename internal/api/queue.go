package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/humanfmt"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
	"github.com/hobeone/gonzbd/internal/unwanted"
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
	if s.downloads != nil {
		s.downloads.PauseDownloads()
	}
	s.log.Info("downloads paused")
	respondStatus(w)
}

func (s *Server) queueResumeAll(w http.ResponseWriter, _ *http.Request) {
	if s.downloads != nil {
		// ResumeDownloads asks for the stall re-evaluation itself (R19), as
		// the low-disk auto-resume does through the same path.
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
	// IngestAnomaly, FailReason, DuplicateReason and OperationalError are
	// Header's four single-owner replacements for what used to be one
	// mutable Warning string — see dispatch.Header's field comments for who
	// writes each and when. Sent verbatim; the client combines them with
	// live queue state (e.g. Status) where that combination matters, rather
	// than the server pre-joining them into prose.
	IngestAnomaly    string `json:"ingest_anomaly,omitempty"`
	FailReason       string `json:"fail_reason,omitempty"`
	DuplicateReason  string `json:"duplicate_reason,omitempty"`
	OperationalError string `json:"operational_error,omitempty"`
	FailedBytes      int64  `json:"failed_bytes"`
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

	// Labels is SABnzbd's per-slot label list. gonzbd sends one label,
	// "UNWANTED", when the unwanted-extension check flagged the job, whether
	// it is still blocked or the user has approved it, as SABnzbd does for
	// any non-zero unwanted_ext. Always a list, never null.
	Labels []string `json:"labels"`

	// CurrentStage is a lowercase machine-readable stage identifier
	// derived from Status (download, repair, unpack, sort, move, ...).
	// Distinct from Status (which is the human-readable label) so the UI
	// can switch on stage without case- or text-fragility.
	CurrentStage string `json:"current_stage"`

	// ArticlesRemaining is the count of articles not yet completed.
	// Reflects job.PendingArticles, which is updated on every state
	// mutation (downloaded, failed, retried).
	//
	// This is the "articles outstanding" count: an article is resolved by
	// being written (Done) or by a permanent failure, and a crash re-fetches
	// what is left here plus any Done article whose row had not been flushed
	// (docs/durability-contract.md § "The state of an article").
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
	// Sourced from the application's in-memory stall map rather than from a
	// Header field: R19's re-evaluation needs a list of every parked job,
	// which only that map (not a per-row field) can provide.
	StallReason string `json:"stall_reason"`

	// BytesDurable keeps its JSON key but means bytes WRITTEN, not fsynced:
	// the NZB-declared size of each resolved article, in yEnc-ENCODED bytes.
	// A power loss can lose written bytes the OS had not yet flushed. It pairs
	// with size/sizeleft/mb, which are the encoded NZB figures beside it.
	BytesDurable int64 `json:"bytes_durable"`

	// ProgressVerified is true when every file of the job has been checked
	// against what is on disk (Job.AllVerified). False means Percentage and
	// the remaining figures are an estimate from the download record. A
	// registered job with no progress record (its job_files structure could
	// not be restored; the tick fails it at first hydration) reads false.
	// An added key: SABnzbd clients ignore unknown slot fields.
	ProgressVerified bool `json:"progress_verified"`

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
	// Verified is whether this file has been checked against what is on disk
	// (JobProgress.FileVerified); false means BytesDownloaded is from the
	// download record.
	Verified bool `json:"verified"`
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
	ft := j.Files()
	p := j.Progress()
	if ft == nil || p == nil {
		return ""
	}
	for i := range ft.NumFiles() {
		if p.FileFetchPolicy(i) != job.FetchAlways {
			continue
		}
		if !p.FileComplete(i) {
			return ft.Subject(i)
		}
	}
	return ""
}

// fileState classifies a file into a coarse UI state. "downloading"
// fires once any article in the file has completed; before that the
// file is "queued". "failed" wins over "done" when any article failed.
func fileState(ft *job.FileTable, p *job.JobProgress, fileIdx int) string {
	switch p.FileFetchPolicy(fileIdx) {
	case job.FetchIfNeeded:
		return "held"
	case job.FetchNever:
		return "skipped"
	}
	if p.FileComplete(fileIdx) {
		anyFailed := false
		lo, hi := ft.Range(fileIdx)
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
	ft := j.Files()
	p := j.Progress()
	if ft == nil || p == nil {
		return []queueFile{}
	}
	out := make([]queueFile, 0, ft.NumFiles())
	for fi := range ft.NumFiles() {
		out = append(out, queueFile{
			Name:            ft.Subject(fi),
			Bytes:           ft.Bytes(fi),
			BytesDownloaded: p.FileBytesDownloaded(fi),
			State:           fileState(ft, p, fi),
			Verified:        p.FileVerified(fi),
		})
	}
	return out
}

// slotLabels returns the queue slot's label list (see queueSlot.Labels).
func slotLabels(h dispatch.Header) []string {
	if h.Unwanted != unwanted.StateNone {
		return []string{"UNWANTED"}
	}
	return []string{}
}

// noiseFloorBPS is the speed below which ETA computation is suppressed
// (returns 0). Random fluctuations in BPS would otherwise produce wildly
// varying ETAs (e.g. 100 hours when the meter dips for a moment).
const noiseFloorBPS = 1024.0 // 1 KiB/s

// queueStatus derives the queue-level status from the whole queue, before
// any listing filter, taking the first rule that applies:
//
//  1. the global pause is on: "Paused", whether or not the queue is empty;
//  2. a job is downloading: "Downloading";
//  3. a job is waiting to download and not paused: "Downloading", since it
//     starts when a slot frees;
//  4. a job is individually paused: "Paused";
//  5. otherwise, including an empty queue or only post-processing jobs:
//     "Idle".
//
// downloading reports rule 2 alone, which is what queueTimeleft gates on.
//
// A running job is skipped before Reason is read, because RenderView
// documents Reason as meaningless while Running is true.
//
// It reads the RenderView axes rather than branching on Row.Status(), which
// dispatch documents as render-only. A job is downloading when it is running
// at Fetching: the case job.ToSABnzbd renders as Downloading or Fetching.
// Running already excludes a settled attempt (sched.Queue.running requires
// Snapshot.IsOpen, which requires an unsettled Outcome).
func queueStatus(rows []dispatch.Row, paused bool) (status string, downloading bool) {
	if paused {
		return "Paused", false
	}
	var waiting, held bool
	for _, r := range rows {
		v := r.View
		switch {
		case v.Running && v.State == job.Fetching:
			return "Downloading", true
		case v.Running || v.Outcome.IsSettled():
		case v.Reason.IsPause():
			held = true
		case waitingToDownload(v):
			waiting = true
		}
	}
	switch {
	case waiting:
		return "Downloading", false
	case held:
		return "Paused", false
	}
	return "Idle", false
}

// waitingToDownload reports whether a job that is neither running, settled
// nor paused is waiting to fetch articles: not yet started (StateUnset), or at
// Fetching waiting for a lease. A job at Fetching whose Next is set has
// finished downloading and is waiting to move on, so it does not count.
func waitingToDownload(v job.RenderView) bool {
	return v.Next == job.StateUnset && (v.State == job.StateUnset || v.State == job.Fetching)
}

// queueSpeedFields returns the queue-level speed and kbpersec for speed, the
// request's snapshot in bytes per second.
func queueSpeedFields(speed float64) (speedStr, kbPerSec string) {
	return formatSpeed(speed), strconv.FormatFloat(speed/(1<<10), 'f', 1, 64)
}

// queueTimeleft returns the listing's queue-level timeleft: remaining over
// speed as h:mm:ss, or "0:00:00" unless downloading is true, speed is above
// noiseFloorBPS and something remains. downloading is queueStatus's second
// result: whether a job is actually running at Fetching (its rule 2), not
// whether the status string reads Downloading — a queue that only has jobs
// waiting for a slot reads Downloading but has no ETA. It is false under the
// global pause, so a speed snapshot still decaying after the last download
// stopped yields no ETA.
func queueTimeleft(speed float64, remaining int64, downloading bool) string {
	if !downloading || speed <= noiseFloorBPS || remaining <= 0 {
		return "0:00:00"
	}
	return formatDuration(int(float64(remaining) / speed))
}

// buildSlot renders one Row into the API queueSlot shape. paused is the
// queue-wide pause flag; speed is the snapshot aggregate BPS used for
// ETA. index is the slot's display index in the listing (0 for the
// detail endpoint). stallReason is why the job is parked, which lives in the
// application rather than in the queue, snapshotted once per request.
func buildSlot(r dispatch.Row, j *job.Job, paused bool, speed float64, index int, duStatus *directunpack.Status, stallReason string) queueSlot {
	// remainingBytes is initialized from r.RemainingBytes, which the Dispatcher
	// captured at listing time. If the job was unregistered between
	// List() and Job(r.ID) (a transient race window), we retain that snapshot
	// rather than resetting to Header.Bytes so progress never falsely flashes 0%.
	// A job with no progress (an unrestorable one; see Dispatcher.List) keeps
	// the header's total.
	totalBytes := r.Header.Bytes
	remainingBytes := r.RemainingBytes
	var failedBytes int64
	// A row whose job is already gone is being removed: nothing is left to
	// verify, so it reads verified rather than flashing the estimate styling.
	progressVerified := true
	if j != nil {
		progressVerified = j.AllVerified()
		if j.HasProgress() {
			totalBytes, remainingBytes, failedBytes = j.ProgressFigures()
		}
	}

	var pct int
	if totalBytes > 0 {
		pct = int(100 * (totalBytes - remainingBytes) / totalBytes)
		if pct < 0 {
			pct = 0
		} else if pct > 100 {
			pct = 100
		}
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
		if j.HasProgress() {
			pendingArticles = j.PendingArticles()
			par2ReleaseReason = j.Par2ReleaseReason()
			durableBytes = app.DurableBytesOf(j)
		}
	}

	return queueSlot{
		NzoID:             r.ID,
		Filename:          r.Header.Filename,
		Name:              r.Header.Name,
		Category:          r.Header.Category,
		Index:             index,
		Priority:          r.Header.Priority.String(),
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
		PP:                strconv.Itoa(r.Header.PP.Int()),
		IngestAnomaly:     r.Header.IngestAnomaly,
		FailReason:        r.Header.FailReason,
		DuplicateReason:   r.Header.DuplicateReason,
		OperationalError:  r.Header.OperationalError,
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
		Labels:            slotLabels(r.Header),
		StallReason:       stallReason,
		BytesDurable:      durableBytes,
		ProgressVerified:  progressVerified,
	}
}

// filterQueueSlots applies the category/status/search filters to rows and
// builds the resulting queueSlot list.
func (s *Server) filterQueueSlots(rows []dispatch.Row, catFilter, statusFilter, searchLower string, paused bool, speed float64, duStatuses map[string]directunpack.Status, stallReasons map[string]string) []queueSlot {
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
		slots = append(slots, buildSlot(r, j, paused, speed, len(slots), duStatus, stallReasons[r.ID]))
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
	var stallReasons map[string]string
	if s.status != nil {
		duStatuses = s.status.DirectUnpackStatuses()
		stallReasons = s.status.StallReasons()
	}

	var slots []queueSlot
	var paused bool
	qStatus := "Idle"
	var downloading bool
	if s.dispatcher != nil {
		rows := s.dispatcher.List()
		paused = s.dispatcher.Paused()
		qStatus, downloading = queueStatus(rows, paused)
		slots = s.filterQueueSlots(rows, catFilter, statusFilter, searchLower, paused, speed, duStatuses, stallReasons)
	}

	total := len(slots)

	// Ensure slots is never nil so JSON encodes as [] not null.
	if slots == nil {
		slots = []queueSlot{}
	}

	// Queue-level size totals cover every filtered slot, so they are summed
	// here, before start/limit cut the page out of slots.
	var totalQueueBytes, remainQueueBytes int64
	for _, sl := range slots {
		totalQueueBytes += sl.Bytes
		remainQueueBytes += sl.RemainingBytes
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

	speedStr, kbPerSec := queueSpeedFields(speed)
	timeleft := queueTimeleft(speed, remainQueueBytes, downloading)
	respondJSON(w, http.StatusOK, queueResponse{
		Status: true,
		Queue: queueDetail{
			Status:         qStatus,
			Paused:         paused,
			Speed:          speedStr,
			KBPerSec:       kbPerSec,
			MB:             toMBString(totalQueueBytes),
			MBLeft:         toMBString(remainQueueBytes),
			Size:           humanfmt.Bytes(totalQueueBytes),
			SizeLeft:       humanfmt.Bytes(remainQueueBytes),
			Timeleft:       timeleft,
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
	var stallReason string
	if s.status != nil {
		if status, ok := s.status.DirectUnpackStatus(nzoID); ok {
			duStatus = &status
		}
		stallReason = s.status.StallReason(nzoID).Reason
	}
	j, _ := s.dispatcher.Job(nzoID)
	slot := buildSlot(row, j, paused, speed, 0, duStatus, stallReason)
	if j != nil {
		slot.Files = buildQueueFiles(j)
	}

	// The response describes this one job, so its timeleft is the slot's,
	// which buildSlot leaves at "0:00:00" unless the job is Downloading.
	speedStr, kbPerSec := queueSpeedFields(speed)
	timeleft := slot.Timeleft
	respondJSON(w, http.StatusOK, queueResponse{
		Status: true,
		Queue: queueDetail{
			Status:         slot.Status,
			Paused:         paused,
			Speed:          speedStr,
			KBPerSec:       kbPerSec,
			Timeleft:       timeleft,
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
	_ = s.queueSetPaused(w, r, "paused")
}

// queueResumeJobs resumes specific jobs by ID (CSV in value=).
func (s *Server) queueResumeJobs(w http.ResponseWriter, r *http.Request) {
	if !s.queueSetPaused(w, r, "resumed") {
		return
	}
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
// IDs are silently ignored, matching SABnzbd's lenient bulk semantics. Any
// other refusal is logged at Warn, and the response is the same success.
//
// A resume is also the approval of a job the unwanted-extension check
// blocked, and approval needs the full API key: a caller with only the NZB
// key who names a blocked job is refused with 403, and no job is resumed.
// It reports whether the request was carried out.
func (s *Server) queueSetPaused(w http.ResponseWriter, r *http.Request, verb string) bool {
	value, ok := s.requireParam(w, r, "value", "")
	if !ok {
		return false
	}
	ids := splitCSV(value)
	approve := verb != "paused" && s.canApproveUnwanted(r)
	if verb != "paused" && !approve && s.dispatcher != nil {
		// ResumeJob enforces the block itself. This only turns what would be
		// a silent refusal into a 403 that names the job.
		for _, id := range ids {
			if row, ok := s.dispatcher.Row(id); ok && row.Header.Unwanted == unwanted.StateBlocked {
				s.respondError(w, http.StatusForbidden, "job "+id+" is blocked for unwanted extensions: "+errApproveNeedsAPIKey)
				return false
			}
		}
	}
	for _, id := range ids {
		var name string
		if s.dispatcher != nil {
			var err error
			switch {
			case verb == "paused":
				err = s.dispatcher.PauseJob(id)
			case approve:
				err = s.dispatcher.ResumeJobByUser(id)
			default:
				err = s.dispatcher.ResumeJob(id)
			}
			if errors.Is(err, dispatch.ErrNotFound) {
				continue
			}
			if err != nil {
				// Logged rather than reported as done.
				s.log.Warn("job not "+verb, "job", id, "err", err)
				continue
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
	return true
}

// queuePriority handles name=priority. SABnzbd convention:
// value = nzo_id, value2 = numeric priority. The response's position is the
// job's index in the queue listing after the change.
func (s *Server) queuePriority(w http.ResponseWriter, r *http.Request) {
	nzoID, ok := s.requireParam(w, r, "value", "nzo_id")
	if !ok {
		return
	}
	raw, ok := s.requireParam(w, r, "value2", "priority")
	if !ok {
		return
	}
	requested, err := parsePriority(raw)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.dispatcher == nil {
		s.respondError(w, http.StatusInternalServerError, "dispatcher not wired")
		return
	}
	// An unknown job reads as category "", and the write below answers 404.
	row, _ := s.dispatcher.Row(nzoID)
	if requested == constants.PausedPriority {
		// A queued job already holds a concrete priority, which a pause keeps.
		err = s.dispatcher.PauseJob(nzoID)
	} else {
		err = s.applyPriority(nzoID, row.Header.Category, requested)
	}
	if err != nil {
		s.respondError(w, priorityErrorStatus(err), err.Error())
		return
	}
	row, _ = s.dispatcher.Row(nzoID)
	s.log.Info("job priority changed", "job", nzoID, "requested", requested.String(),
		"priority", row.Header.Priority.String())
	respondJSON(w, http.StatusOK, map[string]any{
		"status":   true,
		"nzo_ids":  []string{nzoID},
		"position": s.listingPosition(nzoID),
	})
}

// priorityErrorStatus is the HTTP status for an error from setting a job's
// priority: 404 for a job the dispatcher does not hold, 500 otherwise.
func priorityErrorStatus(err error) int {
	if errors.Is(err, dispatch.ErrNotFound) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// applyPriority gives the job what config.ResolvePriority makes of requested
// under category, and pauses it when the resolution says to, as ingest does
// for a new job. The pause comes first: SetPriority kicks the tick, which
// could otherwise lease the job at its new rank, and a refused pause leaves
// the priority as it was.
func (s *Server) applyPriority(id, category string, requested constants.Priority) error {
	var cats []config.CategoryConfig
	if s.config != nil {
		cats = s.config.GetCategories()
	}
	pri, pause, err := config.ResolvePriority(cats, category, requested)
	if err != nil {
		return err
	}
	if pause {
		if err := s.dispatcher.PauseJob(id); err != nil {
			return err
		}
	}
	return s.dispatcher.SetPriority(id, pri)
}

// listingPosition is the job's index in the unfiltered queue listing
// (Dispatcher.List, which is in dispatch order), or -1 — SABnzbd's answer for
// an unknown job — when it is no longer listed.
func (s *Server) listingPosition(id string) int {
	return slices.IndexFunc(s.dispatcher.List(), func(r dispatch.Row) bool { return r.ID == id })
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
	// A negative level is refused, as SABnzbd's change_opts refuses one; a
	// level above 3 runs as 3 (docs/sabnzbd_spec.md §8.2).
	level := intParam(r, "value2")
	if level < 0 {
		s.respondError(w, http.StatusBadRequest, fmt.Sprintf("pp must not be negative, got %d", level))
		return
	}
	pp := job.ClampPP(level)
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
	s.log.Info("job PP changed", "job", nzoID, "pp", pp.Int())
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
		// Resolved as ingest resolves it, so a category at DefaultPriority
		// stores the inherited rank rather than -100, and one at Paused
		// pauses the job. First, so a refused pause or an unknown job changes
		// nothing else.
		if err := s.applyPriority(nzoID, cat, constants.DefaultPriority); err != nil {
			s.respondError(w, priorityErrorStatus(err), err.Error())
			return
		}
		if err := errors.Join(
			s.dispatcher.SetCategory(nzoID, cat),
			s.dispatcher.SetPP(nzoID, job.ClampPP(resolvedCat.PP)),
			s.dispatcher.SetScript(nzoID, resolvedCat.Script),
		); err != nil {
			s.respondError(w, priorityErrorStatus(err), err.Error())
			return
		}
		row, _ := s.dispatcher.Row(nzoID)
		s.log.Info("job category changed", "job", nzoID,
			"cat", row.Header.Category, "pp", row.Header.PP.Int(), "script", row.Header.Script, "priority", row.Header.Priority)
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
	if s.dispatcher == nil || s.jobs == nil {
		s.respondError(w, http.StatusInternalServerError, "dispatcher not wired")
		return
	}
	// The name becomes the job's download directory; RenameJob owns
	// choosing a safe, unique one.
	name, err := s.jobs.RenameJob(nzoID, name)
	switch {
	case errors.Is(err, app.ErrInvalidJobName), errors.Is(err, dispatch.ErrInvalidJobName):
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, dispatch.ErrJobStarted):
		s.respondError(w, http.StatusConflict, "cannot rename a job whose download has started")
		return
	case errors.Is(err, dispatch.ErrNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		s.respondError(w, http.StatusInternalServerError, err.Error())
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
	priority, err := priorityParam(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	opts := types.FetchOptions{
		Category: formValue(r, "cat"),      //nolint:gosec // body size bounded by MaxBytesReader middleware
		Password: formValue(r, "password"), //nolint:gosec // body size bounded by MaxBytesReader middleware
		NzbName:  formValue(r, "nzbname"),  //nolint:gosec // body size bounded by MaxBytesReader middleware
		PP:       ppParam(r),
		Script:   sanitizeScriptParam(formValue(r, "script")), //nolint:gosec // body size bounded by MaxBytesReader middleware
		Priority: priority,
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
	priority, err := priorityParam(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
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
		Priority: priority,
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
