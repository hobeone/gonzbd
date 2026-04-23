package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/history"
)

// modeHistory handles mode=history with sub-actions via the name= parameter.
// Mirrors Python's _api_history and _api_history_table dispatch.
func (s *Server) modeHistory(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		respondError(w, http.StatusInternalServerError, "history not wired")
		return
	}

	action := formString(r, "name")
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
		respondError(w, http.StatusBadRequest, "unknown history action: "+action)
	}
}

// historySlot is the per-entry JSON shape expected by clients.
// Field names must match Python's build_history response exactly.
type historySlot struct {
	NzoID        string `json:"nzo_id"`
	Name         string `json:"name"`
	NZBName      string `json:"nzb_name"`
	Status       string `json:"status"`
	Category     string `json:"category"`
	Script       string `json:"script"`
	FailMsg      string `json:"fail_message"`
	Storage      string `json:"storage"`
	Path         string `json:"path"`
	Size         string `json:"size"`
	Bytes        int64  `json:"bytes"`
	DownloadTime int64  `json:"download_time"`
	Completed    int64  `json:"completed"`
	ScriptLog    string `json:"script_log"`
	ScriptLine   string `json:"script_line"`
	Meta         string `json:"meta"`
	URLInfo      string `json:"url_info"`
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
	limit := intParam(r, "limit")
	search := formString(r, "search")
	catFilter := formString(r, "cat")
	statusFilter := formString(r, "status")
	failedOnly := formString(r, "failed_only") == "1"
	nzoIDs := formString(r, "nzo_ids") // comma-separated IDs to fetch

	opts := history.SearchOptions{
		Start:    start,
		Limit:    limit,
		Search:   search,
		Category: catFilter,
	}
	if statusFilter != "" {
		opts.Status = statusFilter
	}
	if failedOnly {
		opts.Status = "Failed"
	}

	entries, err := s.history.Search(r.Context(), opts)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "history search: "+err.Error())
		return
	}

	// Optional post-filter on specific nzo_ids.
	var nzoIDSet map[string]struct{}
	if nzoIDs != "" {
		nzoIDSet = make(map[string]struct{})
		for _, id := range splitCSV(nzoIDs) {
			nzoIDSet[id] = struct{}{}
		}
	}

	var slots []historySlot
	var totalBytes int64
	for _, e := range entries {
		if nzoIDSet != nil {
			if _, ok := nzoIDSet[e.NzoID]; !ok {
				continue
			}
		}
		totalBytes += e.Bytes
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
			Size:         formatBytes(e.Bytes),
			Bytes:        e.Bytes,
			DownloadTime: e.DownloadTime,
			Completed:    toUnixTS(e.Completed),
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
			NoOfSlots: len(slots),
			TotalSize: formatBytes(totalBytes),
			Slots:     slots,
		},
	})
}

// historyDelete removes history entries. value= may be a CSV of NZO IDs,
// or one of the special tokens: "all", "failed", "completed".
// If delete_files=1 is present, also deletes downloaded files from disk.
func (s *Server) historyDelete(w http.ResponseWriter, r *http.Request) {
	value := formString(r, "value")
	if value == "" {
		respondError(w, http.StatusBadRequest, "missing value parameter")
		return
	}

	deleteFiles := r.FormValue("delete_files") == "1"

	var ids []string

	switch strings.ToLower(value) {
	case "all":
		entries, err := s.history.Search(r.Context(), history.SearchOptions{})
		if err != nil {
			respondError(w, http.StatusInternalServerError, "history search: "+err.Error())
			return
		}
		for _, e := range entries {
			ids = append(ids, e.NzoID)
		}
	case "failed":
		entries, err := s.history.Search(r.Context(), history.SearchOptions{Status: "Failed"})
		if err != nil {
			respondError(w, http.StatusInternalServerError, "history search: "+err.Error())
			return
		}
		for _, e := range entries {
			ids = append(ids, e.NzoID)
		}
	case "completed":
		entries, err := s.history.Search(r.Context(), history.SearchOptions{Status: "Completed"})
		if err != nil {
			respondError(w, http.StatusInternalServerError, "history search: "+err.Error())
			return
		}
		for _, e := range entries {
			ids = append(ids, e.NzoID)
		}
	default:
		ids = splitCSV(value)
	}

	var deleted int
	for _, id := range ids {
		if err := s.app.RemoveHistoryJob(r.Context(), id, deleteFiles); err == nil {
			deleted++
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
	nzoID := formString(r, "value")
	if nzoID == "" {
		respondError(w, http.StatusBadRequest, "missing value parameter")
		return
	}

	if err := s.history.MarkCompleted(r.Context(), nzoID); err != nil {
		respondError(w, http.StatusInternalServerError, "mark completed: "+err.Error())
		return
	}
	respondStatus(w)
}

// historyRetry moves an entry from history back to the queue via the retry
// sub-action. The nzo_id is supplied in the value= parameter.
func (s *Server) historyRetry(w http.ResponseWriter, r *http.Request) {
	nzoID := formString(r, "value")
	if nzoID == "" {
		respondError(w, http.StatusBadRequest, "missing value parameter")
		return
	}

	if err := s.app.RetryHistoryJob(r.Context(), nzoID); err != nil {
		respondError(w, http.StatusInternalServerError, "retry: "+err.Error())
		return
	}
	respondStatus(w)
}

// toUnixTS returns t.Unix() or 0 for zero times.
func toUnixTS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
