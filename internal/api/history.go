package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/humanfmt"
)

// modeHistory handles mode=history with sub-actions via the name= parameter.
// Mirrors Python's _api_history and _api_history_table dispatch.
func (s *Server) modeHistory(w http.ResponseWriter, r *http.Request) {
	if !s.requireHistory(w) {
		return
	}

	action := formValue(r, "name")
	switch action {
	case "", "list":
		s.historyList(w, r)
	case "delete":
		s.historyDelete(w, r)
	case "retry":
		s.historyRetry(w, r)
	case "mark_as_completed":
		s.historyMarkCompleted(w, r)
	default:
		s.respondError(w, http.StatusBadRequest, "unknown history action: "+action)
	}
}

// historySlot is the per-entry JSON shape expected by clients.
// Field names must match Python's build_history response exactly.
type historySlot struct {
	NzoID        string          `json:"nzo_id"`
	Name         string          `json:"name"`
	NZBName      string          `json:"nzb_name"`
	Status       string          `json:"status"`
	Category     string          `json:"category"`
	Script       string          `json:"script"`
	FailMsg      string          `json:"fail_message"`
	Storage      string          `json:"storage"`
	Path         string          `json:"path"`
	Size         string          `json:"size"`
	Bytes        int64           `json:"bytes"`
	Downloaded   int64           `json:"downloaded"`
	Completeness int64           `json:"completeness"`
	DownloadTime int64           `json:"download_time"`
	PostprocTime int64           `json:"postproc_time"`
	Completed    int64           `json:"completed"`
	StageLog     []stageLogEntry `json:"stage_log"`
	ScriptLog    string          `json:"script_log"`
	ScriptLine   string          `json:"script_line"`
	Meta         string          `json:"meta"`
	URLInfo      string          `json:"url_info"`
}

// stageLogEntry represents a post-processing stage result (Repair, Unpack, etc.).
type stageLogEntry struct {
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// historyResponse is the outer JSON object returned for history listings.
type historyResponse struct {
	Status  bool          `json:"status"`
	History historyDetail `json:"history"`
}

// historyDetail is the nested object under "history" in the listing response.
type historyDetail struct {
	NoOfSlots int           `json:"noofslots"`
	TotalSize string        `json:"total_size"`
	Slots     []historySlot `json:"slots"`
}

// historyList returns a paginated, filtered history listing.
func (s *Server) historyList(w http.ResponseWriter, r *http.Request) {
	start := intParam(r, "start")
	rawLimit := formValue(r, "limit")
	limit := intParam(r, "limit")
	search := formValue(r, "search")
	catFilter := formValue(r, "cat")
	statusFilter := formValue(r, "status")
	failedOnly := formValue(r, "failed_only") == "1"
	nzoIDs := formValue(r, "nzo_ids") // comma-separated IDs to fetch

	// Default limit prevents loading the entire DB into memory.
	// Max cap prevents OOM from adversarial limit= values.
	const defaultLimit = 50
	const maxLimit = 10_000
	if rawLimit == "" {
		// No limit param at all → use default.
		limit = defaultLimit
	} else if limit <= 0 {
		// limit=0 means "return all" (Sonarr sends this).
		limit = maxLimit
	}
	limit = min(limit, maxLimit)

	opts := history.SearchOptions{
		Search:   search,
		Category: catFilter,
	}
	if statusFilter != "" {
		opts.Status = statusFilter
	}
	if failedOnly {
		opts.Status = "Failed"
	}
	var entries []history.Entry

	if nzoIDs != "" {
		entries = s.fetchEntriesByIDs(r.Context(), nzoIDs, catFilter, statusFilter, search, failedOnly)
	} else {
		opts.Start = start
		opts.Limit = limit
		var err error
		entries, err = s.history.Search(r.Context(), opts)
		if err != nil {
			s.respondError(w, http.StatusInternalServerError, "history search: "+err.Error())
			return
		}
	}

	// Total count for pagination (always reflects the unfiltered-by-ID total).
	totalCount, err := s.history.Count(r.Context(), opts)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "history count: "+err.Error())
		return
	}

	slots := make([]historySlot, 0, len(entries))
	var totalBytes int64

	for _, e := range entries {
		totalBytes += e.Bytes

		// Deserialize the stored stage log JSON. Fall back to an empty
		// array on any error (legacy entries may have empty or invalid JSON).
		stages := parseStageLog(e.StageLog)

		slots = append(slots, historySlot{
			NzoID:        e.NzoID,
			Name:         e.Name,
			NZBName:      e.NzbName,
			Status:       e.Status,
			Category:     e.Category,
			Script:       e.Script,
			FailMsg:      e.FailMessage,
			Storage:      e.Storage,
			Path:         e.Path,
			Size:         humanfmt.Bytes(e.Bytes),
			Bytes:        e.Bytes,
			Downloaded:   e.Downloaded,
			Completeness: e.Completeness,
			DownloadTime: e.DownloadTime,
			PostprocTime: e.PostprocTime,
			Completed:    toUnixTS(e.Completed),
			StageLog:     stages,
			ScriptLog:    string(e.ScriptLog),
			ScriptLine:   e.ScriptLine,
			Meta:         e.Meta,
			URLInfo:      e.URLInfo,
		})
	}

	if slots == nil {
		slots = []historySlot{}
	}

	respondJSON(w, http.StatusOK, historyResponse{
		Status: true,
		History: historyDetail{
			NoOfSlots: totalCount,
			TotalSize: humanfmt.Bytes(totalBytes),
			Slots:     slots,
		},
	})
}

// fetchEntriesByIDs retrieves specific history entries by their NZO IDs,
// applying manual filters since we bypass the database search query.
func (s *Server) fetchEntriesByIDs(
	ctx context.Context,
	nzoIDs string,
	catFilter, statusFilter, search string,
	failedOnly bool,
) []history.Entry {
	var entries []history.Entry
	for _, id := range splitCSV(nzoIDs) {
		e, err := s.history.Get(ctx, id)
		if err != nil || e == nil {
			continue
		}
		if catFilter != "" && e.Category != catFilter {
			continue
		}
		if statusFilter != "" && e.Status != statusFilter {
			continue
		}
		if failedOnly && e.Status != "Failed" {
			continue
		}
		if search != "" {
			sLower := strings.ToLower(search)
			if !strings.Contains(strings.ToLower(e.Name), sLower) &&
				!strings.Contains(strings.ToLower(e.NzbName), sLower) {
				continue
			}
		}
		entries = append(entries, *e)
	}
	return entries
}

// historyDelete removes history entries. value= may be a CSV of NZO IDs,
// or one of the special tokens: "all", "failed", "completed".
// If delete_files=1 is present, also deletes downloaded files from disk.
func (s *Server) historyDelete(w http.ResponseWriter, r *http.Request) {
	value, ok := s.requireParam(w, r, "value", "")
	if !ok {
		return
	}

	deleteFiles := formValue(r, "delete_files") == "1" || formValue(r, "del_files") == "1" //nolint:gosec // body size bounded by MaxBytesReader middleware

	var ids []string

	switch strings.ToLower(value) {
	case "all":
		var err error
		ids, err = s.history.ListIDs(r.Context(), history.SearchOptions{})
		if err != nil {
			s.respondError(w, http.StatusInternalServerError, "history list ids: "+err.Error())
			return
		}
	case "failed":
		var err error
		ids, err = s.history.ListIDs(r.Context(), history.SearchOptions{Status: "Failed"})
		if err != nil {
			s.respondError(w, http.StatusInternalServerError, "history list ids: "+err.Error())
			return
		}
	case "completed":
		var err error
		ids, err = s.history.ListIDs(r.Context(), history.SearchOptions{Status: "Completed"})
		if err != nil {
			s.respondError(w, http.StatusInternalServerError, "history list ids: "+err.Error())
			return
		}
	default:
		ids = splitCSV(value)
	}

	var deleted int
	for _, id := range ids {
		if err := s.jobs.RemoveHistoryJob(r.Context(), id, deleteFiles); err == nil {
			deleted++
		} else {
			s.log.WarnContext(r.Context(), "failed to remove job during bulk delete", "id", id, "error", err)
		}
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"status":  true,
		"deleted": deleted,
	})
}

// historyMarkCompleted marks an entry as completed via the mark_as_completed
// sub-action. The nzo_id is supplied in the value= parameter.
func (s *Server) historyMarkCompleted(w http.ResponseWriter, r *http.Request) {
	nzoID, ok := s.requireParam(w, r, "value", "")
	if !ok {
		return
	}

	if err := s.history.MarkCompleted(r.Context(), nzoID); err != nil {
		s.respondError(w, http.StatusInternalServerError, "mark completed: "+err.Error())
		return
	}
	respondStatus(w)
}

// historyRetry moves an entry from history back to the queue via the retry
// sub-action. The nzo_id is supplied in the value= parameter.
func (s *Server) historyRetry(w http.ResponseWriter, r *http.Request) {
	nzoID, ok := s.requireParam(w, r, "value", "")
	if !ok {
		return
	}

	if err := s.jobs.RetryHistoryJob(r.Context(), nzoID); err != nil {
		s.respondError(w, http.StatusInternalServerError, "retry: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"status": true,
		"nzo_id": nzoID,
	})
}

// toUnixTS returns t.Unix() or 0 for zero times.
func toUnixTS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// internalStageLogEntry mirrors the postproc.StageLogEntry struct for
// JSON deserialization of the stored stage log. We define a separate
// type here to avoid importing the postproc package and to handle the
// error-as-string conversion.
type internalStageLogEntry struct {
	Stage   string   `json:"Stage"`
	Elapsed float64  `json:"Elapsed"` // nanoseconds (time.Duration)
	Err     *string  `json:"Err"`     // nil or error string
	Lines   []string `json:"Lines"`
}

// parseStageLog deserializes the JSON-encoded stage log string from the
// database into API-friendly stageLogEntry values. Returns an empty (non-nil)
// slice on any error or empty input.
func parseStageLog(raw string) []stageLogEntry {
	if raw == "" || raw == "null" {
		return []stageLogEntry{}
	}

	var internal []internalStageLogEntry
	if err := json.Unmarshal([]byte(raw), &internal); err != nil {
		return []stageLogEntry{}
	}

	result := make([]stageLogEntry, 0, len(internal))
	for _, e := range internal {
		actions := make([]string, 0, len(e.Lines)+1)

		// Add duration as the first action line.
		elapsedSec := e.Elapsed / 1e9 // nanoseconds → seconds
		if elapsedSec >= 60 {
			mins := int(elapsedSec) / 60
			secs := int(elapsedSec) % 60
			actions = append(actions, fmt.Sprintf("Completed in %dm %ds", mins, secs))
		} else if elapsedSec > 0 {
			actions = append(actions, fmt.Sprintf("Completed in %.1fs", elapsedSec))
		}

		if e.Err != nil && *e.Err != "" {
			actions = append(actions, "Error: "+*e.Err)
		}
		actions = append(actions, e.Lines...)

		result = append(result, stageLogEntry{
			Name:    e.Stage,
			Actions: actions,
		})
	}

	if len(result) == 0 {
		return []stageLogEntry{}
	}
	return result
}
