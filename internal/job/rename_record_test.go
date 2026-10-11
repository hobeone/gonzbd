package job

import (
	"errors"
	"testing"
)

// TestSetNameAndRecordFileFilename_OneWins pins the two refusals that order a
// rename against a file's filename (#838): a filename resolved under a name
// SetName has since replaced is refused with ErrNameChanged and recorded
// nowhere, and once a filename is recorded SetName refuses with
// ErrDownloadBegun and keeps the name.
func TestSetNameAndRecordFileFilename_OneWins(t *testing.T) {
	t.Parallel()
	j := New("id", "old", Policy{})
	m := NewManifest([]JobFile{{Subject: "f", Bytes: 100, Articles: []JobArticle{{ID: "<a@x>", Bytes: 100, Number: 1}}}})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	if err := j.SetName("new"); err != nil {
		t.Fatalf("SetName before any filename: %v", err)
	}
	if err := j.RecordFileFilename(0, "old", "f"); !errors.Is(err, ErrNameChanged) {
		t.Fatalf("RecordFileFilename under the replaced name = %v, want ErrNameChanged", err)
	}
	if j.DownloadBegun() {
		t.Fatal("a refused RecordFileFilename recorded the filename")
	}

	if err := j.RecordFileFilename(0, "new", "f"); err != nil {
		t.Fatalf("RecordFileFilename under the current name: %v", err)
	}
	if err := j.SetName("newer"); !errors.Is(err, ErrDownloadBegun) {
		t.Fatalf("SetName after a filename = %v, want ErrDownloadBegun", err)
	}
	if got := j.Name(); got != "new" {
		t.Errorf("a refused SetName changed the name to %q", got)
	}
}
