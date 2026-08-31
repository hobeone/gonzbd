package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestBuildHistoryEntry_CarriesNZBBackup pins that the backup basename
// crosses the queue→history boundary. It is the only link from a history
// entry back to the NZB, so dropping it here strands the file: present on
// disk, unreachable from the entry that needs it.
func TestBuildHistoryEntry_CarriesNZBBackup(t *testing.T) {
	t.Parallel()
	entry := buildHistoryEntry(&postproc.Job{
		Queue: &queue.Job{
			ID:        "carrybackup00001",
			Filename:  "Show.S01E01.nzb",
			NZBBackup: "Show.S01E01.nzb.1.gz",
			Name:      "Show.S01E01",
		},
	})

	if entry.NZBBackup != "Show.S01E01.nzb.1.gz" {
		t.Errorf("NZBBackup = %q, want %q", entry.NZBBackup, "Show.S01E01.nzb.1.gz")
	}
	if entry.NzbName != "Show.S01E01.nzb" {
		t.Errorf("NzbName = %q, want %q", entry.NzbName, "Show.S01E01.nzb")
	}
}
