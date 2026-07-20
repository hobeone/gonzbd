package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/api/apitest"
	"github.com/hobeone/gonzbd/internal/config"
)

type spyHealthApp struct {
	apitest.NopApp
	pingErr        error
	freeBytes      int64
	freeErr        error
	pipelineHealth bool
}

func (a *spyHealthApp) PingDB(context.Context) error {
	return a.pingErr
}

func (a *spyHealthApp) DownloadDirFreeBytes(context.Context) (int64, error) {
	return a.freeBytes, a.freeErr
}

func (a *spyHealthApp) IsPipelineHealthy(context.Context) bool {
	return a.pipelineHealth
}

func TestModeHealth_LevelOpen_Healthy(t *testing.T) {
	app := &spyHealthApp{
		freeBytes:      1000000000,
		pipelineHealth: true,
	}
	srv := New(Options{App: app})

	req := httptest.NewRequest(http.MethodGet, "/api?mode=health", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("json parse error: %v", err)
	}

	if res["status"] != "ok" {
		t.Errorf("status = %v, want 'ok'", res["status"])
	}
	if len(res) != 1 {
		t.Errorf("unauthenticated response leaked extra fields: %v", res)
	}
}

func TestModeHealth_LevelOpen_Unhealthy_ZeroLeakage(t *testing.T) {
	app := &spyHealthApp{
		pingErr:        errors.New("secret_db_path.sqlite: connection refused"),
		freeBytes:      1000000000,
		pipelineHealth: true,
	}
	srv := New(Options{App: app})

	req := httptest.NewRequest(http.MethodGet, "/api?mode=health", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}

	body := rec.Body.String()
	if strings.Contains(body, "secret_db_path") || strings.Contains(body, "connection refused") {
		t.Errorf("unauthenticated 503 leaked internal details: %s", body)
	}

	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("json parse error: %v", err)
	}

	if res["status"] != "error" {
		t.Errorf("status = %v, want 'error'", res["status"])
	}
	if len(res) != 1 {
		t.Errorf("unauthenticated response leaked extra fields: %v", res)
	}
}

func TestModeHealth_LevelProtected_Detailed(t *testing.T) {
	app := &spyHealthApp{
		freeBytes:      1000000000,
		pipelineHealth: true,
	}
	cfg := &config.Config{General: config.GeneralConfig{APIKey: testAPIKey}}
	srv := New(Options{
		App:    app,
		Config: cfg,
	})

	req := httptest.NewRequest(http.MethodGet, "/api?mode=health&apikey="+testAPIKey, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("json parse error: %v", err)
	}

	if res["status"] != true {
		t.Errorf("status = %v, want true", res["status"])
	}
	health, ok := res["health"].(map[string]any)
	if !ok {
		t.Fatalf("health map missing or invalid: %v", res)
	}
	if health["db"] != "ok" || health["disk"] != "ok" || health["pipeline"] != "ok" {
		t.Errorf("health details = %v, want all ok", health)
	}
}

func TestModeHealth_LevelProtected_Unhealthy_DiskSpace(t *testing.T) {
	app := &spyHealthApp{
		freeBytes:      50, // lower than min_free_space
		pipelineHealth: true,
	}
	cfg := &config.Config{
		General: config.GeneralConfig{APIKey: testAPIKey},
	}
	cfg.Downloads.MinFreeSpace = config.ByteSize(100)
	srv := New(Options{
		App:    app,
		Config: cfg,
	})

	req := httptest.NewRequest(http.MethodGet, "/api?mode=health&apikey="+testAPIKey, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}

	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("json parse error: %v", err)
	}

	if res["status"] != false {
		t.Errorf("status = %v, want false", res["status"])
	}
	health, ok := res["health"].(map[string]any)
	if !ok {
		t.Fatalf("health map missing or invalid: %v", res)
	}
	if health["disk"] != "low disk space" {
		t.Errorf("disk status = %v, want 'low disk space'", health["disk"])
	}
}
