package app

import (
	"bytes"
	"compress/gzip"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/history"
)

// rebuildTestApp returns an Application wired with just enough to run
// rebuildJobFromNZB: a config with an admin dir, and a logger.
func rebuildTestApp(t *testing.T) (*Application, string) {
	t.Helper()
	adminDir := t.TempDir()
	return &Application{
		config: &config.Config{General: config.GeneralConfig{AdminDir: adminDir}},
		log:    slog.New(slog.DiscardHandler),
	}, adminDir
}

// writeBackup gzips body into adminDir/nzb/<name>, creating the directory.
func writeBackup(t *testing.T, adminDir, name string, body []byte) {
	t.Helper()
	dir := filepath.Join(adminDir, "nzb")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir nzb: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(body); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write backup: %v", err)
	}
}

const rebuildNZBBody = `<?xml version="1.0" encoding="iso-8859-1" ?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
<file poster="p@t" date="1700000000" subject="&quot;file.bin&quot; yEnc (1/1)">
<groups><group>alt.bin.test</group></groups>
<segments>
<segment bytes="1024" number="1">seg1@t</segment>
<segment bytes="1024" number="2">seg2@t</segment>
</segments>
</file>
</nzb>
`

// TestRebuildJobFromNZB_Success pins that the rebuilt job takes the entry's
// identity and options rather than whatever NewJob would have generated. The
// ID in particular has to match, or the retained progress keyed on it and the
// job's existing incomplete directory both go unfound.
func TestRebuildJobFromNZB_Success(t *testing.T) {
	a, adminDir := rebuildTestApp(t)
	writeBackup(t, adminDir, "show.nzb.gz", []byte(rebuildNZBBody))

	job, err := a.rebuildJobFromNZB(&history.Entry{
		NzoID:     "rebuildok0000001",
		Name:      "show",
		NzbName:   "show.nzb",
		NZBBackup: "show.nzb.gz",
		Category:  "tv",
		Script:    "post.sh",
		Password:  "pw",
		PP:        "3",
	})
	if err != nil {
		t.Fatalf("rebuildJobFromNZB: %v", err)
	}
	if job.ID != "rebuildok0000001" {
		t.Errorf("ID = %q, want the entry's nzo_id", job.ID)
	}
	if job.NZBBackup != "show.nzb.gz" {
		t.Errorf("NZBBackup = %q, want it carried onto the rebuilt job", job.NZBBackup)
	}
	if job.Category != "tv" || job.Script != "post.sh" || job.Password != "pw" || job.PP != 3 {
		t.Errorf("options lost: category=%q script=%q password=%q pp=%d",
			job.Category, job.Script, job.Password, job.PP)
	}
	if job.NumArticles() != 2 {
		t.Errorf("NumArticles = %d, want 2 — the NZB was not parsed", job.NumArticles())
	}
}

// TestRebuildJobFromNZB_UnparseablePP pins that a malformed pp falls back to
// inheriting from the category rather than failing the retry. The column is
// written by us, but a hand-edited database should degrade, not block.
func TestRebuildJobFromNZB_UnparseablePP(t *testing.T) {
	a, adminDir := rebuildTestApp(t)
	writeBackup(t, adminDir, "show.nzb.gz", []byte(rebuildNZBBody))

	job, err := a.rebuildJobFromNZB(&history.Entry{
		NzoID:     "rebuildpp0000001",
		Name:      "show",
		NZBBackup: "show.nzb.gz",
		PP:        "not-a-number",
	})
	if err != nil {
		t.Fatalf("rebuildJobFromNZB: %v", err)
	}
	if job == nil {
		t.Fatal("nil job for an unparseable pp")
	}
}

// TestRebuildJobFromNZB_Failures pins that each way of not getting an NZB
// reports which one it was. These are the paths a retry hits when the backup
// is absent or unreadable, and an opaque error here is the difference between
// "re-add the NZB by hand" and "no idea why retry did nothing".
func TestRebuildJobFromNZB_Failures(t *testing.T) {
	tests := []struct {
		name    string
		backup  string
		setup   func(t *testing.T, adminDir string)
		wantErr string
	}{
		{
			name:    "no backup recorded",
			backup:  "",
			wantErr: "no NZB backup was recorded",
		},
		{
			name:    "backup file missing",
			backup:  "absent.nzb.gz",
			wantErr: "open NZB backup",
		},
		{
			name:   "not gzip",
			backup: "plain.nzb.gz",
			setup: func(t *testing.T, adminDir string) {
				t.Helper()
				dir := filepath.Join(adminDir, "nzb")
				if err := os.MkdirAll(dir, 0o750); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "plain.nzb.gz"), []byte("not gzip"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
			wantErr: "read NZB backup",
		},
		{
			name:   "gzip of non-NZB",
			backup: "junk.nzb.gz",
			setup: func(t *testing.T, adminDir string) {
				t.Helper()
				writeBackup(t, adminDir, "junk.nzb.gz", []byte("<html>nope</html>"))
			},
			wantErr: "parse NZB backup",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, adminDir := rebuildTestApp(t)
			if tt.setup != nil {
				tt.setup(t, adminDir)
			}
			_, err := a.rebuildJobFromNZB(&history.Entry{
				NzoID:     "rebuildfail00001",
				Name:      "x",
				NZBBackup: tt.backup,
			})
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestRebuildJobFromNZB_IgnoresPathSeparators pins that a stored backup name
// is resolved as a basename inside admin/nzb/, so a value containing
// separators cannot reach outside it. The column is ours, but the database is
// a file on disk and not a trust boundary we control after the fact.
func TestRebuildJobFromNZB_IgnoresPathSeparators(t *testing.T) {
	a, adminDir := rebuildTestApp(t)
	writeBackup(t, adminDir, "show.nzb.gz", []byte(rebuildNZBBody))

	job, err := a.rebuildJobFromNZB(&history.Entry{
		NzoID:     "rebuildbase00001",
		Name:      "show",
		NZBBackup: "../../../show.nzb.gz",
	})
	if err != nil {
		t.Fatalf("rebuildJobFromNZB: %v", err)
	}
	if job.NumArticles() != 2 {
		t.Errorf("NumArticles = %d, want 2", job.NumArticles())
	}
}
