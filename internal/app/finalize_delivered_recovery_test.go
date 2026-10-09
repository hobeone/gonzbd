package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/testutil"
)

// TestEnqueuePostProc_DeliveredCrashRecovery verifies #767 end-to-end through
// Application.enqueuePostProc -> PostProcessor -> buildHistoryEntry:
//   - For a per-job category destination (catDir not ending in "*"), when
//     DownloadDir is missing and FinalDir exists and is non-empty, the job is
//     treated as already delivered: finalize is skipped, script runs against
//     FinalDir, and history records Status == "Completed" with Path == FinalDir.
//   - For a flat category destination (catDir ending in "*"), enqueuePostProc
//     sets FlatLayout = true on the postproc.Job, so a non-empty shared
//     FinalDir does not rescue the missing DownloadDir and the job is filed
//     Failed with Path == DownloadDir.
func TestEnqueuePostProc_DeliveredCrashRecovery(t *testing.T) {
	t.Parallel()

	runCase := func(t *testing.T, catDir, scriptBody string) (entryStatus, entryPath, entryStorage, entryStageLog, scriptStatus, scriptDirOut, downloadDir, finalDir string) {
		t.Helper()

		scriptDir := t.TempDir()
		statusFile := filepath.Join(scriptDir, "status.txt")
		dirFile := filepath.Join(scriptDir, "dir.txt")
		testutil.WriteExecutable(t, filepath.Join(scriptDir, "notify.sh"),
			"#!/bin/sh\necho \"$SAB_PP_STATUS\" > "+statusFile+"\necho \"$SAB_FINAL_PROCESSING_DIR\" > "+dirFile+"\n"+scriptBody)

		scriptStage := postproc.NewScriptStage(scriptDir, "", "test", "", "")
		scriptStage.SetScriptCanFail(true)
		application, repo, _ := newLifecycleTestApp(t, WithPostProcStages([]postproc.Stage{
			postproc.NewFinalizeStage(),
			scriptStage,
		}))
		application.ctx = t.Context()
		application.config.With(func(c *config.Config) {
			c.PostProc.ScriptCanFail = true
			c.Categories = []config.CategoryConfig{
				{Name: "movies", Dir: catDir},
			}
		})

		runner := holdingRunner{launched: make(chan string, 1)}
		d := dispatch.New(
			1, 1, 10*time.Millisecond, time.Now,
			&appWorkers{app: application},
			application.residency,
			dispatchstore.New(repo.DB(), nil),
			runner,
		)
		application.dispatcher = d
		application.pipeline.dispatcher = d

		j, hdr := jobAt(t, application, "MyRelease", job.Extracting)
		hdr.Category = "movies"
		hdr.Script = "notify.sh"

		if err := d.Add(t.Context(), j, hdr); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if err := d.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { stopHeldDispatcher(d) })
		if err := application.postProcessor.Start(t.Context()); err != nil {
			t.Fatalf("postProcessor.Start: %v", err)
		}
		t.Cleanup(func() { _ = application.postProcessor.Stop() })

		select {
		case <-runner.launched:
		case <-time.After(10 * time.Second):
			t.Fatal("Extracting job was never launched")
		}

		gen := application.config.GetGeneral()
		downloadDir = filepath.Join(gen.DownloadDir, j.Name())
		// Deliberately leave downloadDir missing (already moved before crash)
		// and populate finalDir instead.
		cleanCat := strings.TrimSuffix(catDir, "*")
		if strings.HasSuffix(catDir, "*") {
			finalDir = filepath.Join(gen.CompleteDir, cleanCat)
		} else {
			finalDir = filepath.Join(gen.CompleteDir, cleanCat, j.Name())
		}
		if err := os.MkdirAll(finalDir, 0o750); err != nil {
			t.Fatalf("mkdir finalDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(finalDir, "movie.mkv"), []byte("video"), 0o600); err != nil {
			t.Fatalf("write movie.mkv: %v", err)
		}

		application.enqueuePostProc(j, hdr, "", false)
		awaitFinalized(t, application, j.ID())

		entry := historyEntry(t, application, j.ID())
		gotStatus, _ := os.ReadFile(statusFile)
		gotDir, _ := os.ReadFile(dirFile)
		return entry.Status, entry.Path, entry.Storage, entry.StageLog,
			strings.TrimSpace(string(gotStatus)),
			strings.TrimSpace(string(gotDir)),
			downloadDir, finalDir
	}

	t.Run("per-job category completes and runs script", func(t *testing.T) {
		t.Parallel()
		status, path, storage, stageLog, scriptStatus, scriptDirOut, _, finalDir := runCase(t, "movies", "")
		if status != "Completed" {
			t.Errorf("history Status = %q, want %q", status, "Completed")
		}
		if path != finalDir {
			t.Errorf("history Path = %q, want FinalDir %q", path, finalDir)
		}
		if storage != finalDir {
			t.Errorf("history Storage = %q, want FinalDir %q", storage, finalDir)
		}
		if strings.Contains(stageLog, "Error reading download dir") {
			t.Errorf("StageLog contains read error on already-delivered job: %s", stageLog)
		}
		if strings.Contains(stageLog, `"Stage":"finalize"`) {
			t.Errorf("finalize stage ran on already-delivered job; StageLog = %s", stageLog)
		}
		if !strings.Contains(stageLog, `"Stage":"script"`) {
			t.Errorf("script stage did not run on already-delivered job; StageLog = %s", stageLog)
		}
		if scriptStatus != "0" {
			t.Errorf("script SAB_PP_STATUS = %q, want %q", scriptStatus, "0")
		}
		if scriptDirOut != finalDir {
			t.Errorf("script SAB_FINAL_PROCESSING_DIR = %q, want FinalDir %q", scriptDirOut, finalDir)
		}
	})

	t.Run("per-job category with failing script files Failed with Path and Storage at FinalDir", func(t *testing.T) {
		t.Parallel()
		status, path, storage, stageLog, scriptStatus, scriptDirOut, _, finalDir := runCase(t, "movies", "exit 7\n")
		if status != "Failed" {
			t.Errorf("history Status = %q, want %q", status, "Failed")
		}
		if path != finalDir {
			t.Errorf("history Path = %q, want FinalDir %q", path, finalDir)
		}
		if storage != finalDir {
			t.Errorf("history Storage = %q, want FinalDir %q", storage, finalDir)
		}
		if strings.Contains(stageLog, "Error reading download dir") {
			t.Errorf("StageLog contains read error on already-delivered job: %s", stageLog)
		}
		if scriptStatus != "0" {
			t.Errorf("script SAB_PP_STATUS = %q, want %q", scriptStatus, "0")
		}
		if scriptDirOut != finalDir {
			t.Errorf("script SAB_FINAL_PROCESSING_DIR = %q, want FinalDir %q", scriptDirOut, finalDir)
		}
	})

	t.Run("flat category dir ending in * still files Failed", func(t *testing.T) {
		t.Parallel()
		status, path, storage, _, scriptStatus, _, downloadDir, _ := runCase(t, "movies*", "")
		if status != "Failed" {
			t.Errorf("history Status = %q, want %q", status, "Failed")
		}
		if path != downloadDir {
			t.Errorf("history Path = %q, want missing DownloadDir %q", path, downloadDir)
		}
		if storage != downloadDir {
			t.Errorf("history Storage = %q, want missing DownloadDir %q", storage, downloadDir)
		}
		if scriptStatus != "" {
			t.Errorf("script ran on flat-layout missing-DownloadDir job (status = %q)", scriptStatus)
		}
	})
}
