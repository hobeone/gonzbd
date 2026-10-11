package job

import "testing"

// TestFileLocation pins what FileLocation returns: the job's current name and
// the file's recorded filename, with "" for a file with none, an index that
// names no file, and a job with no progress.
func TestFileLocation(t *testing.T) {
	t.Parallel()
	j := New("id", "old", Policy{})
	if name, fn := j.FileLocation(0); name != "old" || fn != "" {
		t.Errorf("no progress: FileLocation = (%q, %q), want (old, \"\")", name, fn)
	}
	m := NewManifest([]JobFile{{Subject: "f", Bytes: 100, Articles: []JobArticle{{ID: "<a@x>", Bytes: 100, Number: 1}}}})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if err := j.SetName("new"); err != nil {
		t.Fatalf("SetName: %v", err)
	}
	if name, fn := j.FileLocation(0); name != "new" || fn != "" {
		t.Errorf("no filename: FileLocation = (%q, %q), want (new, \"\")", name, fn)
	}
	if err := j.RecordFileFilename(0, "new", "on-disk.bin"); err != nil {
		t.Fatalf("RecordFileFilename: %v", err)
	}
	if name, fn := j.FileLocation(0); name != "new" || fn != "on-disk.bin" {
		t.Errorf("FileLocation = (%q, %q), want (new, on-disk.bin)", name, fn)
	}
	if _, fn := j.FileLocation(1); fn != "" {
		t.Errorf("out of range: filename %q, want none", fn)
	}
}
