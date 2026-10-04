package api

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/buildinfo"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// downloadDirRig is a Server wired to a real Application, so a refusal is the
// application's and not a spy's.
type downloadDirRig struct {
	s           *Server
	application *app.Application
	cfg         *config.Config
	cfgPath     string
	oldDir      string
}

func newDownloadDirRig(t *testing.T) *downloadDirRig {
	t.Helper()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	root := t.TempDir()
	cfg.With(func(c *config.Config) {
		c.General.APIKey = testAPIKey
		c.General.AdminDir = filepath.Join(root, "admin")
		c.General.DownloadDir = filepath.Join(root, "incomplete")
		c.General.CompleteDir = filepath.Join(root, "complete")
		c.Servers = []config.ServerConfig{{Name: "mock", Host: "127.0.0.1", Port: 1119, Enable: false, Connections: 1, Timeout: 60, PipeliningRequests: 1}}
	})
	for _, d := range []string{cfg.GetGeneral().AdminDir, cfg.GetGeneral().DownloadDir, cfg.GetGeneral().CompleteDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	application, err := app.New(cfg, nil)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	cfgPath := filepath.Join(root, "gonzbd.yaml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s := New(Options{
		Build:      buildinfo.Info{Version: "1.0.0-test"},
		Config:     cfg,
		App:        application,
		ConfigPath: cfgPath,
	})
	return &downloadDirRig{s: s, application: application, cfg: cfg, cfgPath: cfgPath, oldDir: cfg.GetGeneral().DownloadDir}
}

func (r *downloadDirRig) queueJob(t *testing.T) {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "a.bin",
		Bytes:    10,
		Articles: []nzb.Article{{Bytes: 10, ID: "a@example.com", Number: 1}},
	}}}
	j, hdr, err := app.BuildIngestJob(r.cfg, parsed, "a.nzb", types.FetchOptions{NzbName: "queued", PP: 3}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := r.application.AddJob(t.Context(), j, hdr, nil, false); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
}

func (r *downloadDirRig) setDownloadDir(t *testing.T, dir string) (code int, body string) {
	t.Helper()
	rr := apiGet(t, r.s.Handler(), "/api?mode=set_config&section=general&keyword=download_dir&value="+url.QueryEscape(dir)+"&apikey="+testAPIKey)
	return rr.Code, rr.Body.String()
}

func TestSetConfigDownloadDir_RefusedWithAQueuedJob(t *testing.T) {
	t.Parallel()
	r := newDownloadDirRig(t)
	r.queueJob(t)
	savedBefore, err := os.ReadFile(r.cfgPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	next := filepath.Join(t.TempDir(), "elsewhere")

	code, body := r.setDownloadDir(t, next)

	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body: %s)", code, body)
	}
	if !strings.Contains(body, "unfinished jobs") {
		t.Errorf("body = %q, want it to name the queue as the reason", body)
	}
	if got := r.cfg.GetGeneral().DownloadDir; got != r.oldDir {
		t.Errorf("live config download_dir = %q, want the old %q", got, r.oldDir)
	}
	savedAfter, err := os.ReadFile(r.cfgPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(savedBefore) != string(savedAfter) {
		t.Error("the config file on disk changed on a refused download_dir change")
	}
	if _, err := os.Stat(next); err == nil {
		t.Errorf("the refused directory %q was created", next)
	}
}

func TestSetConfigDownloadDir_AppliesWithAnEmptyQueue(t *testing.T) {
	t.Parallel()
	r := newDownloadDirRig(t)
	next := filepath.Join(t.TempDir(), "elsewhere")

	code, body := r.setDownloadDir(t, next)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", code, body)
	}
	if got := r.cfg.GetGeneral().DownloadDir; got != next {
		t.Errorf("live config download_dir = %q, want %q", got, next)
	}
	if _, err := os.Stat(next); err != nil {
		t.Errorf("download_dir %q was not created: %v", next, err)
	}
}

func TestSetConfigDownloadDir_SameValueSucceedsWithAQueuedJob(t *testing.T) {
	t.Parallel()
	r := newDownloadDirRig(t)
	r.queueJob(t)

	code, body := r.setDownloadDir(t, r.oldDir)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", code, body)
	}
}

// TestSetConfigDownloadDir_InvalidValueIsStillABadRequest pins that the trial
// validation precedes the refusal, so an empty value is not reported as a
// queue problem.
func TestSetConfigDownloadDir_InvalidValueIsStillABadRequest(t *testing.T) {
	t.Parallel()
	r := newDownloadDirRig(t)
	r.queueJob(t)

	code, body := r.setDownloadDir(t, "")

	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", code, body)
	}
}
