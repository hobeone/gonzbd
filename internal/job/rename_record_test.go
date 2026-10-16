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

// TestRecordFileFilename_RecordsWhenEvicted pins #854: the filename lives on
// the progress record, which eviction keeps, so an evicted job records it, and
// an index outside the progress record's files is refused without a write.
func TestRecordFileFilename_RecordsWhenEvicted(t *testing.T) {
	t.Parallel()
	j := New("id", "name", Policy{})
	m := NewManifest([]JobFile{{Subject: "f", Bytes: 100, Articles: []JobArticle{{ID: "<a@x>", Bytes: 100, Number: 1}}}})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	j.Evict()

	if err := j.RecordFileFilename(0, "name", "on-disk.bin"); err != nil {
		t.Fatalf("RecordFileFilename on an evicted job: %v", err)
	}
	if got := j.Progress().FileFilename(0); got != "on-disk.bin" {
		t.Errorf("recorded filename %q, want on-disk.bin", got)
	}
	for _, fi := range []int{-1, 1} {
		if err := j.RecordFileFilename(fi, "name", "x"); err == nil {
			t.Errorf("RecordFileFilename(%d) on a one-file job succeeded, want out of range", fi)
		}
	}
}

// TestJob_RecordFileRename pins that RecordFileRename updates the recorded
// on-disk filename of a file in place, marks the file dirty for recorder
// flush, and refuses invalid or multi-component paths.
func TestJob_RecordFileRename(t *testing.T) {
	t.Parallel()
	j := New("id", "name", Policy{})
	m := NewManifest([]JobFile{
		{Subject: "f1", Bytes: 100, Articles: []JobArticle{{ID: "<a1@x>", Bytes: 100, Number: 1}}},
		{Subject: "f2", Bytes: 200, Articles: []JobArticle{{ID: "<a2@x>", Bytes: 200, Number: 2}}},
	})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	// Before any filename is recorded, renaming fails.
	if j.RecordFileRename("orig.rar", "renamed.rar") {
		t.Fatal("RecordFileRename succeeded before any filename was recorded")
	}

	if err := j.RecordFileFilename(0, "name", "orig.rar"); err != nil {
		t.Fatalf("RecordFileFilename: %v", err)
	}
	if err := j.RecordFileFilename(1, "name", "keep.rar"); err != nil {
		t.Fatalf("RecordFileFilename: %v", err)
	}
	// Drain initial fetch policy changes.
	_ = j.TakeFetchPolicyChanges()

	// Invalid names or multi-component paths are refused.
	for _, bad := range []struct {
		from, to string
		desc     string
	}{
		{"orig.rar", "orig.rar", "identical"},
		{"orig.rar", "", "empty to"},
		{"", "renamed.rar", "empty from"},
		{"orig.rar", ".", "dot to"},
		{".", "renamed.rar", "dot from"},
		{"orig.rar", "..", "dotdot to"},
		{"..", "renamed.rar", "dotdot from"},
		{"orig.rar", "sub/renamed.rar", "slash to"},
		{"sub/orig.rar", "renamed.rar", "slash from"},
		{"orig.rar", `sub\renamed.rar`, "backslash to"},
		{`sub\orig.rar`, "renamed.rar", "backslash from"},
		{"nonexistent.rar", "renamed.rar", "unknown from"},
	} {
		if j.RecordFileRename(bad.from, bad.to) {
			t.Errorf("RecordFileRename(%q, %q) [%s] = true, want false", bad.from, bad.to, bad.desc)
		}
	}

	// Valid in-place rename.
	if !j.RecordFileRename("orig.rar", "renamed.rar") {
		t.Fatal("RecordFileRename(orig.rar, renamed.rar) = false, want true")
	}
	if got := j.Progress().FileFilename(0); got != "renamed.rar" {
		t.Errorf("FileFilename(0) = %q, want renamed.rar", got)
	}
	if got := j.Progress().FileFilename(1); got != "keep.rar" {
		t.Errorf("FileFilename(1) = %q, want keep.rar", got)
	}

	// TakeFetchPolicyChanges must hand the updated filename to the record.
	changes := j.TakeFetchPolicyChanges()
	if len(changes) != 1 {
		t.Fatalf("TakeFetchPolicyChanges returned %d changes, want 1", len(changes))
	}
	if changes[0].FileIdx != 0 || changes[0].Filename != "renamed.rar" {
		t.Errorf("TakeFetchPolicyChanges change[0] = %+v, want FileIdx:0 Filename:renamed.rar", changes[0])
	}
}

// TestIsSinglePathComponent pins that isSinglePathComponent accepts exactly single
// path components, refusing empty, dot, dot-dot, and paths with slashes.
func TestIsSinglePathComponent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"", false},
		{".", false},
		{"..", false},
		{"a/b", false},
		{"a\\b", false},
		{"file.bin", true},
		{"archive.part01.rar", true},
	} {
		if got := isSinglePathComponent(tc.name); got != tc.want {
			t.Errorf("isSinglePathComponent(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
