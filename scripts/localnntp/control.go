// Package main implements the localnntp release generator, NNTP server, and crash verifier.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type durationValue time.Duration

func (d *durationValue) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*d = 0
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		if s == "" {
			*d = 0
			return nil
		}
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = durationValue(parsed)
		return nil
	}
	var ns int64
	if err := json.Unmarshal(trimmed, &ns); err != nil {
		return fmt.Errorf("duration must be a string (e.g. \"5s\") or integer nanoseconds: %w", err)
	}
	*d = durationValue(time.Duration(ns))
	return nil
}

type faultRequest struct {
	Spec         *string       `json:"spec,omitempty"`
	Action       FaultAction   `json:"action,omitempty"`
	MessageIDs   []string      `json:"message_ids,omitempty"`
	FileMatch    string        `json:"file_match,omitempty"`
	Role         FileRole      `json:"role,omitempty"`
	Parts        []int         `json:"parts,omitempty"`
	Rate         float64       `json:"rate,omitempty"`
	CorruptBytes int           `json:"corrupt_bytes,omitempty"`
	TimeoutAfter durationValue `json:"timeout_after,omitempty"`
	MaxFires     int           `json:"max_fires,omitempty"`
}

func decodeFaultRequest(r io.Reader) (FaultRule, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var req faultRequest
	if err := dec.Decode(&req); err != nil {
		return FaultRule{}, fmt.Errorf("invalid json: %w", err)
	}
	if req.Spec != nil {
		if *req.Spec == "" {
			return FaultRule{}, errors.New("spec must be a non-empty string")
		}
		return ParseFaultSpec(*req.Spec)
	}
	rule := FaultRule{
		Action:       req.Action,
		MessageIDs:   req.MessageIDs,
		FileMatch:    req.FileMatch,
		Role:         req.Role,
		Parts:        req.Parts,
		Rate:         req.Rate,
		CorruptBytes: req.CorruptBytes,
		TimeoutAfter: time.Duration(req.TimeoutAfter),
		MaxFires:     req.MaxFires,
	}
	if err := ValidateFaultRule(&rule); err != nil {
		return FaultRule{}, err
	}
	return rule, nil
}

// NewControlHandler constructs an HTTP handler routing the localnntp control API.
func NewControlHandler(srv *Server) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(srv.Stats()) //nolint:errcheck // response writer write failure is handled by net/http
	})

	mux.HandleFunc("POST /faults", func(w http.ResponseWriter, r *http.Request) {
		rule, err := decodeFaultRequest(r.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid fault rule: %v", err), http.StatusBadRequest)
			return
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

	mux.HandleFunc("POST /conn", func(w http.ResponseWriter, r *http.Request) {
		// Fields omitted from the body keep their current value; kill_active is a one-shot action.
		var req struct {
			MaxConnections *int  `json:"max_connections"`
			Refuse         *bool `json:"refuse"`
			RejectGreeting *bool `json:"reject_greeting"`
			AuthFail       *bool `json:"auth_fail"`
			KillActive     bool  `json:"kill_active"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
			return
		}
		if req.MaxConnections != nil && *req.MaxConnections < 0 {
			http.Error(w, "max_connections cannot be negative", http.StatusBadRequest)
			return
		}
		cur := srv.ConnSettings()
		if req.MaxConnections != nil {
			cur.MaxConnections = *req.MaxConnections
		}
		if req.Refuse != nil {
			cur.Refuse = *req.Refuse
		}
		if req.RejectGreeting != nil {
			cur.RejectGreeting = *req.RejectGreeting
		}
		if req.AuthFail != nil {
			cur.AuthFail = *req.AuthFail
		}
		srv.SetConnSettings(cur)
		killed := 0
		if req.KillActive {
			killed = srv.KillActive()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"conn": cur, "killed": killed}) //nolint:errcheck // response writer write failure is handled by net/http
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
