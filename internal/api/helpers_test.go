package api

import (
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/urlgrabber"

	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/types"
)

func TestToMBString(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0.00"},
		{1 << 20, "1.00"},
		{1024 * 1024 * 1024, "1024.00"},
		{1572864, "1.50"}, // 1.5 MB
	}

	for _, tt := range tests {
		if got := toMBString(tt.bytes); got != tt.want {
			t.Errorf("toMBString(%d) = %q; want %q", tt.bytes, got, tt.want)
		}
	}
}

func TestIntParam(t *testing.T) {
	tests := []struct {
		name   string
		urlStr string
		key    string
		want   int
	}{
		{"valid value", "/api?val=123", "val", 123},
		{"invalid value", "/api?val=abc", "val", 0},
		{"missing value", "/api?other=123", "val", 0},
		{"empty value", "/api?val=", "val", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.urlStr, nil)
			if got := intParam(req, tt.key); got != tt.want {
				t.Errorf("intParam() = %d; want %d", got, tt.want)
			}
		})
	}
}

func TestPriorityParam(t *testing.T) {
	tests := []struct {
		name   string
		urlStr string
		want   constants.Priority
	}{
		{"valid priority", "/api?priority=2", constants.Priority(2)},
		{"missing priority", "/api", constants.DefaultPriority},
		{"invalid priority", "/api?priority=abc", constants.NormalPriority},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.urlStr, nil)
			if got := priorityParam(req); got != tt.want {
				t.Errorf("priorityParam() = %v; want %v", got, tt.want)
			}
		})
	}
}

func TestPPParam(t *testing.T) {
	tests := []struct {
		name   string
		urlStr string
		want   int
	}{
		{"valid pp", "/api?pp=2", 2},
		{"missing pp", "/api", types.PPInherit},
		{"invalid pp", "/api?pp=abc", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.urlStr, nil)
			if got := ppParam(req); got != tt.want {
				t.Errorf("ppParam() = %v; want %v", got, tt.want)
			}
		})
	}
}

func TestMutationClassificationStrategies(t *testing.T) {
	t.Parallel()

	sysTests := []struct {
		mode string
		want bool
	}{
		{"set_config", true},
		{"shutdown", true},
		{"restart", true},
		{"pause", true},
		{"resume", true},
		{"disconnect", true},
		{"addurl", true},
		{"addfile", true},
		{"addlocalfile", true},
		{"pause_pp", true},
		{"resume_pp", true},
		{"speedlimit", true},
		{"queue", false},
		{"history", false},
		{"version", false},
	}
	for _, tt := range sysTests {
		if got := isSystemMutationMode(tt.mode); got != tt.want {
			t.Errorf("isSystemMutationMode(%q) = %v; want %v", tt.mode, got, tt.want)
		}
	}

	queueTests := []struct {
		name string
		want bool
	}{
		{"delete", true},
		{"purge", true},
		{"delete_nzf", true},
		{"pause", true},
		{"resume", true},
		{"priority", true},
		{"change_opts", true},
		{"switch", true},
		{"list", false},
		{"", false},
	}
	for _, tt := range queueTests {
		if got := isQueueMutation(tt.name); got != tt.want {
			t.Errorf("isQueueMutation(%q) = %v; want %v", tt.name, got, tt.want)
		}
	}

	histTests := []struct {
		name string
		want bool
	}{
		{"delete", true},
		{"purge", true},
		{"list", false},
		{"", false},
	}
	for _, tt := range histTests {
		if got := isHistoryMutation(tt.name); got != tt.want {
			t.Errorf("isHistoryMutation(%q) = %v; want %v", tt.name, got, tt.want)
		}
	}
}

func TestOpenFileDirect(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test.txt")
	content := []byte("hello")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := openFile(path)
	if err != nil {
		t.Fatalf("openFile failed: %v", err)
	}
	defer f.Close()

	// Test non-existent file
	_, err = openFile(filepath.Join(tmpDir, "missing.txt"))
	if err == nil {
		t.Error("expected error opening missing file, got nil")
	}
}

func TestSplitCSV(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"a, b, c", []string{"a", "b", "c"}},
		{"a,,b,", []string{"a", "b"}},
		{"", []string{}},
		{"   ", []string{}},
	}

	for _, tt := range tests {
		if got := splitCSV(tt.input); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitCSV(%q) = %v; want %v", tt.input, got, tt.want)
		}
	}
}

func TestNonEmpty(t *testing.T) {
	tests := []struct {
		s        string
		fallback string
		want     string
	}{
		{"value", "fallback", "value"},
		{"", "fallback", "fallback"},
	}

	for _, tt := range tests {
		if got := nonEmpty(tt.s, tt.fallback); got != tt.want {
			t.Errorf("nonEmpty(%q, %q) = %q; want %q", tt.s, tt.fallback, got, tt.want)
		}
	}
}

func TestFormatDurationDirect(t *testing.T) {
	tests := []struct {
		seconds int
		want    string
	}{
		{0, "0:00:00"},
		{3661, "1:01:01"},
		{-10, "0:00:00"},
		{86400, "24:00:00"},
	}

	for _, tt := range tests {
		if got := formatDuration(tt.seconds); got != tt.want {
			t.Errorf("formatDuration(%d) = %q; want %q", tt.seconds, got, tt.want)
		}
	}
}

func TestToUnixTS(t *testing.T) {
	tm := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	if got := toUnixTS(tm); got != 1780574400 {
		t.Errorf("toUnixTS(%v) = %d; want 1780574400", tm, got)
	}
}

func TestRequireParamDirect(t *testing.T) {
	s := testServer()

	// Case 1: Parameter is present
	req1 := httptest.NewRequest("GET", "/api?mykey=myval", nil)
	rr1 := httptest.NewRecorder()
	val, ok := s.requireParam(rr1, req1, "mykey", "label")
	if !ok {
		t.Error("expected ok=true when parameter is present")
	}
	if val != "myval" {
		t.Errorf("expected val=\"myval\", got %q", val)
	}

	// Case 2: Parameter is missing
	req2 := httptest.NewRequest("GET", "/api", nil)
	rr2 := httptest.NewRecorder()
	_, ok = s.requireParam(rr2, req2, "mykey", "label")
	if ok {
		t.Error("expected ok=false when parameter is missing")
	}
	if rr2.Code != 400 {
		t.Errorf("expected status 400, got %d", rr2.Code)
	}
}

type recordHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *recordHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *recordHandler) hasWarn(msg string, id string, errSubstr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level != slog.LevelWarn || r.Message != msg {
			continue
		}
		var gotID string
		var gotErr string
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "id" {
				gotID = a.Value.String()
			}
			if a.Key == "error" {
				gotErr = a.Value.String()
			}
			return true
		})
		if gotID == id && strings.Contains(gotErr, errSubstr) {
			return true
		}
	}
	return false
}

func TestRequireDependenciesDirect(t *testing.T) {
	t.Parallel()
	s := &Server{log: slog.Default()} // all dependencies nil, logger non-nil

	tests := []struct {
		name    string
		queue   bool
		app     bool
		cfg     bool
		history bool
		grabber bool
		wantMsg string
	}{
		{"missing queue", true, false, false, false, false, "queue not wired"},
		{"missing app", false, true, false, false, false, "app not wired"},
		{"missing config", false, false, true, false, false, "config not wired"},
		{"missing history", false, false, false, true, false, "history not wired"},
		{"missing grabber", false, false, false, false, true, "url grabber not wired"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			ok := s.requireDependencies(rr, tt.queue, tt.app, tt.cfg, tt.history, tt.grabber)
			if ok {
				t.Errorf("expected false, got true")
			}
			if rr.Code != http.StatusInternalServerError {
				t.Errorf("status = %d; want 500", rr.Code)
			}
			if !strings.Contains(rr.Body.String(), tt.wantMsg) {
				t.Errorf("body = %q; want %q", rr.Body.String(), tt.wantMsg)
			}
		})
	}

	// Also test individual require* methods when nil
	t.Run("requireQueue nil", func(t *testing.T) {
		rr := httptest.NewRecorder()
		if s.requireQueue(rr) || !strings.Contains(rr.Body.String(), "queue not wired") {
			t.Errorf("unexpected result for requireQueue: %v, %s", s.requireQueue(rr), rr.Body.String())
		}
	})
	t.Run("requireApp nil", func(t *testing.T) {
		rr := httptest.NewRecorder()
		if s.requireApp(rr) || !strings.Contains(rr.Body.String(), "app not wired") {
			t.Errorf("unexpected result for requireApp: %v, %s", s.requireApp(rr), rr.Body.String())
		}
	})
	t.Run("requireConfig nil", func(t *testing.T) {
		rr := httptest.NewRecorder()
		if s.requireConfig(rr) || !strings.Contains(rr.Body.String(), "config not wired") {
			t.Errorf("unexpected result for requireConfig: %v, %s", s.requireConfig(rr), rr.Body.String())
		}
	})
	t.Run("requireHistory nil", func(t *testing.T) {
		rr := httptest.NewRecorder()
		if s.requireHistory(rr) || !strings.Contains(rr.Body.String(), "history not wired") {
			t.Errorf("unexpected result for requireHistory: %v, %s", s.requireHistory(rr), rr.Body.String())
		}
	})
	t.Run("requireGrabber nil", func(t *testing.T) {
		rr := httptest.NewRecorder()
		if s.requireGrabber(rr) || !strings.Contains(rr.Body.String(), "url grabber not wired") {
			t.Errorf("unexpected result for requireGrabber: %v, %s", s.requireGrabber(rr), rr.Body.String())
		}
	})

	// Also test when dependencies are non-nil (true path)
	t.Run("dependencies non-nil", func(t *testing.T) {
		s2 := testServer()
		s2.history = &history.Repository{}
		s2.grabber = &urlgrabber.Grabber{}
		rr := httptest.NewRecorder()
		if !s2.requireQueue(rr) || !s2.requireApp(rr) || !s2.requireConfig(rr) || !s2.requireHistory(rr) || !s2.requireGrabber(rr) {
			t.Errorf("expected true when dependencies wired")
		}
	})
}

func TestIsStateChangingMode(t *testing.T) {
	if !isStateChangingMode("queue", "pause") {
		t.Error("isStateChangingMode(queue, pause) = false, want true")
	}
	if isStateChangingMode("queue", "list") {
		t.Error("isStateChangingMode(queue, list) = true, want false")
	}
	if isStateChangingMode("queue", "") {
		t.Error("isStateChangingMode(queue, \"\") = true, want false")
	}
	if !isStateChangingMode("config", "save") {
		t.Error("isStateChangingMode(config, save) = false, want true")
	}
	if !isStateChangingMode("warnings", "clear") {
		t.Error("isStateChangingMode(warnings, clear) = false, want true")
	}
	if isStateChangingMode("warnings", "") {
		t.Error("isStateChangingMode(warnings, \"\") = true, want false")
	}
}
