package app

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// TestApplication_BPSMeterLifetimeTotalsSurviveRestart pins that Application
// owns the persistence of app.meter across a clean Shutdown and subsequent New,
// and that both New and ReloadDownloader wire app.meter into downloader.New:
// wire bytes read by the initial downloader and by a reloaded downloader are
// written to meterStatePath on Shutdown and restored into the new meter on New.
func TestApplication_BPSMeterLifetimeTotalsSurviveRestart(t *testing.T) {
	t.Parallel()

	dlDir := t.TempDir()
	compDir := t.TempDir()
	adminDir := t.TempDir()
	srv := nntptest.New(t)
	cfg := testConfig(dlDir, compDir, adminDir, srv.ServerConfig("primary", 1))

	a1, err := New(cfg, nil, WithMetricsPushInterval(time.Hour))
	if err != nil {
		t.Fatalf("New (first): %v", err)
	}
	if err := a1.Start(t.Context()); err != nil {
		t.Fatalf("Start (first): %v", err)
	}

	const primaryMsgID = "bpsmeter-primary@test"
	j1, hdr1, raw1 := buildTestIngestJob(t, a1, &nzb.NZB{Files: []nzb.File{{
		Subject:  "primary.bin",
		Bytes:    64,
		Articles: []nzb.Article{{ID: primaryMsgID, Bytes: 64, Number: 1}},
	}}}, "bpsmeter-primary")
	if err := a1.AddJob(t.Context(), j1, hdr1, raw1, true); err != nil {
		t.Fatalf("AddJob(primary): %v", err)
	}
	waitPrimaryDeadline := time.Now().Add(5 * time.Second)
	for !j1.Progress().ArticleFailed(0) {
		if time.Now().After(waitPrimaryDeadline) {
			t.Fatalf("timed out waiting for primary article resolution (fetches=%d)", srv.FetchCount(primaryMsgID))
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := a1.ReloadDownloader([]config.ServerConfig{srv.ServerConfig("backup", 1)}); err != nil {
		t.Fatalf("ReloadDownloader: %v", err)
	}

	const backupMsgID = "bpsmeter-backup@test"
	j2, hdr2, raw2 := buildTestIngestJob(t, a1, &nzb.NZB{Files: []nzb.File{{
		Subject:  "backup.bin",
		Bytes:    64,
		Articles: []nzb.Article{{ID: backupMsgID, Bytes: 64, Number: 1}},
	}}}, "bpsmeter-backup")
	if err := a1.AddJob(t.Context(), j2, hdr2, raw2, true); err != nil {
		t.Fatalf("AddJob(backup): %v", err)
	}
	waitBackupDeadline := time.Now().Add(5 * time.Second)
	for !j2.Progress().ArticleFailed(0) {
		if time.Now().After(waitBackupDeadline) {
			t.Fatalf("timed out waiting for backup article resolution (fetches=%d)", srv.FetchCount(backupMsgID))
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := a1.Shutdown(); err != nil {
		t.Fatalf("Shutdown (first): %v", err)
	}

	wantPrimary := a1.meter.Total("primary")
	wantBackup := a1.meter.Total("backup")
	wantTotal := a1.meter.Total("")
	if wantPrimary <= 0 {
		t.Errorf("pre-restart meter.Total(\"primary\") = %d, want > 0", wantPrimary)
	}
	if wantBackup <= 0 {
		t.Errorf("pre-restart meter.Total(\"backup\") = %d, want > 0", wantBackup)
	}

	a2, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New (second): %v", err)
	}

	if got := a2.meter.Total(""); got <= 0 || got != wantTotal {
		t.Errorf("restored meter.Total(\"\") = %d, want %d (> 0)", got, wantTotal)
	}
	if got := a2.meter.Total("primary"); got <= 0 || got != wantPrimary {
		t.Errorf("restored meter.Total(\"primary\") = %d, want %d (> 0)", got, wantPrimary)
	}
	if got := a2.meter.Total("backup"); got <= 0 || got != wantBackup {
		t.Errorf("restored meter.Total(\"backup\") = %d, want %d (> 0)", got, wantBackup)
	}
}

// TestApplication_BPSMeterCorruptStateLogsWarning pins that a corrupt
// bpsmeter.json file at meterStatePath logs a warning during New instead of
// being silently swallowed like os.ErrNotExist.
func TestApplication_BPSMeterCorruptStateLogsWarning(t *testing.T) {
	t.Parallel()

	dlDir := t.TempDir()
	compDir := t.TempDir()
	adminDir := t.TempDir()
	cfg := testConfig(dlDir, compDir, adminDir)

	seed, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New (seed): %v", err)
	}
	if err := os.WriteFile(seed.meterStatePath(), []byte("{corrupt-json"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", seed.meterStatePath(), err)
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	a, err := New(cfg, nil, WithLogger(logger))
	if err != nil {
		t.Fatalf("New (corrupt state): %v", err)
	}
	if got := a.meter.Total(""); got != 0 {
		t.Errorf("meter.Total(\"\") = %d on corrupt state, want 0", got)
	}
	if !strings.Contains(logBuf.String(), "load bpsmeter state") {
		t.Errorf("expected warning %q in log output, got %q", "load bpsmeter state", logBuf.String())
	}
}
