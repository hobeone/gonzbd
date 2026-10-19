package postproc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// scriptEnvAfterRun runs ScriptStage over j with a script that dumps its
// SAB_ environment, and returns it as a map.
func scriptEnvAfterRun(t *testing.T, j *Job) map[string]string {
	t.Helper()
	scriptDir := t.TempDir()
	out := filepath.Join(scriptDir, "env.txt")
	writeScript(t, filepath.Join(scriptDir, "env.sh"),
		fmt.Appendf(nil, "#!/bin/sh\nenv | grep '^SAB_' > %s\n", out))
	j.Script = "env.sh"
	if err := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "").Run(t.Context(), j); err != nil {
		t.Fatalf("script stage: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("script wrote no environment: %v", err)
	}
	env := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		env[k] = v
	}
	return env
}

// TestScriptStage_DownloadFigures pins that SAB_BYTES_DOWNLOADED,
// SAB_BYTES_TRIED, SAB_DOWNLOAD_TIME and SAB_AVG_BPS carry the job's real
// figures (#1014), and SAB_REPAIR and SAB_UNPACK its PP level as flags.
//
// The job is one 1000-byte file of two articles (600 + 400) whose second
// article failed, downloaded over 10 s: 600 bytes written, 60 B/s.
func TestScriptStage_DownloadFigures(t *testing.T) {
	t.Parallel()
	files := []job.JobFile{{Subject: "a.bin", Bytes: 1000, Articles: []job.JobArticle{
		{ID: "a0@t", Bytes: 600, Number: 1},
		{ID: "a1@t", Bytes: 400, Number: 2},
	}}}
	started := time.Unix(1700000000, 0)

	for _, tc := range []struct {
		pp      int
		repair  string
		unpackV string
	}{{0, "0", "0"}, {1, "1", "0"}, {2, "1", "1"}, {3, "1", "1"}} {
		t.Run(fmt.Sprintf("pp%d", tc.pp), func(t *testing.T) {
			t.Parallel()
			j, _ := stageJob(t)
			qj := job.New("fig-id", "fig", job.PolicyFromPP(tc.pp))
			if err := qj.AttachContent(job.NewManifest(files)); err != nil {
				t.Fatal(err)
			}
			if err := qj.MarkArticleFailed(1, job.WholeArticle); err != nil {
				t.Fatal(err)
			}
			if err := qj.MarkFileComplete(0); err != nil {
				t.Fatal(err)
			}
			if err := qj.MarkJobStarted(started); err != nil {
				t.Fatal(err)
			}
			if err := qj.MarkDownloadFinished(started.Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			j.Job, j.PP = qj, tc.pp

			env := scriptEnvAfterRun(t, j)
			for k, want := range map[string]string{
				"SAB_BYTES":            "1000",
				"SAB_BYTES_DOWNLOADED": "600",
				"SAB_BYTES_TRIED":      "1000",
				"SAB_DOWNLOAD_TIME":    "10",
				"SAB_AVG_BPS":          "60",
				"SAB_REPAIR":           tc.repair,
				"SAB_UNPACK":           tc.unpackV,
			} {
				if env[k] != want {
					t.Errorf("%s = %q, want %q", k, env[k], want)
				}
			}
		})
	}
}
