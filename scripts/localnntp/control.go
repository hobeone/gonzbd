// Package main implements the localnntp release generator, NNTP server, and crash verifier.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// NewControlHandler constructs an HTTP handler routing the localnntp control API.
func NewControlHandler(srv *Server) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(srv.Stats()) //nolint:errcheck // response writer write failure is handled by net/http
	})

	mux.HandleFunc("POST /faults", func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]any
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
			return
		}

		var rule FaultRule
		if specVal, ok := raw["spec"]; ok {
			specStr, isStr := specVal.(string)
			if !isStr || specStr == "" {
				http.Error(w, "spec must be a non-empty string", http.StatusBadRequest)
				return
			}
			parsed, err := ParseFaultSpec(specStr)
			if err != nil {
				http.Error(w, fmt.Sprintf("invalid fault spec: %v", err), http.StatusBadRequest)
				return
			}
			rule = parsed
		} else {
			ruleBytes, err := json.Marshal(raw)
			if err != nil {
				http.Error(w, fmt.Sprintf("marshal json rule: %v", err), http.StatusBadRequest)
				return
			}
			if err := json.Unmarshal(ruleBytes, &rule); err != nil {
				http.Error(w, fmt.Sprintf("invalid fault rule: %v", err), http.StatusBadRequest)
				return
			}
			if rule.Action == "" {
				http.Error(w, "missing action in fault rule", http.StatusBadRequest)
				return
			}
		}

		srv.Faults().AddRule(rule)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(srv.Faults().Rules()) //nolint:errcheck // response writer write failure is handled by net/http
	})

	mux.HandleFunc("DELETE /faults", func(w http.ResponseWriter, r *http.Request) {
		srv.Faults().ClearRules()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /rate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RateMBps float64 `json:"rate_mbps"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
			return
		}
		if req.RateMBps < 0 {
			http.Error(w, "rate_mbps cannot be negative", http.StatusBadRequest)
			return
		}
		srv.Limiter().SetRateMBps(req.RateMBps)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"rate_mbps": req.RateMBps}) //nolint:errcheck // response writer write failure is handled by net/http
	})

	mux.HandleFunc("POST /snapshot", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name        string   `json:"name"`
			RecordedIDs []string `json:"recorded_ids"`
		}
		name := r.URL.Query().Get("name")
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
				http.Error(w, "invalid JSON payload: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		if name == "" {
			name = req.Name
		}
		if name == "" {
			http.Error(w, "snapshot name is required", http.StatusBadRequest)
			return
		}
		snap := srv.TakeSnapshot(name, req.RecordedIDs)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snap) //nolint:errcheck // response writer write failure is handled by net/http
	})

	mux.HandleFunc("GET /snapshot", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "snapshot name query parameter is required", http.StatusBadRequest)
			return
		}
		snap, ok := srv.GetSnapshot(name)
		if !ok {
			http.Error(w, "snapshot not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snap) //nolint:errcheck // response writer write failure is handled by net/http
	})

	return mux
}
