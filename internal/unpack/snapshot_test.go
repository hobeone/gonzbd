package unpack

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestSnapshotDirAndDiff(t *testing.T) {
	tmp := t.TempDir()

	// 1. Empty snapshot
	snap1, err := snapshotDir(tmp)
	if err != nil {
		t.Fatalf("snapshotDir failed: %v", err)
	}
	if len(snap1) != 0 {
		t.Errorf("expected empty snapshot, got %v", snap1)
	}

	// 2. Create some files
	f1 := filepath.Join(tmp, "file1.txt")
	if err := os.WriteFile(f1, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	f2 := filepath.Join(tmp, "subdir", "file2.txt")
	if err := os.Mkdir(filepath.Dir(f2), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, []byte("world"), 0600); err != nil {
		t.Fatal(err)
	}

	snap2, err := snapshotDir(tmp)
	if err != nil {
		t.Fatalf("snapshotDir failed: %v", err)
	}
	expectedSnap2 := map[string]struct{}{
		"file1.txt":        {},
		"subdir/file2.txt": {},
	}
	if !reflect.DeepEqual(snap2, expectedSnap2) {
		t.Errorf("got snapshot %v, want %v", snap2, expectedSnap2)
	}

	// 3. Diff
	diff := diffSnapshot(snap1, snap2)
	slices.Sort(diff)
	expectedDiff := []string{"file1.txt", "subdir/file2.txt"}
	if !reflect.DeepEqual(diff, expectedDiff) {
		t.Errorf("diffSnapshot = %v, want %v", diff, expectedDiff)
	}

	// 4. No new files
	diffEmpty := diffSnapshot(snap2, snap2)
	if len(diffEmpty) != 0 {
		t.Errorf("expected empty diff, got %v", diffEmpty)
	}

	// 5. Reverse diff (files present in earlier snapshot but absent in later)
	diffReverse := diffSnapshot(snap2, snap1)
	if len(diffReverse) != 0 {
		t.Errorf("diffSnapshot(snap2, snap1) = %v, want empty (only new files reported)", diffReverse)
	}
}
