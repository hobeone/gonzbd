package job

import "testing"

// TestMarkFileComplete_MarksWhenEvicted pins #868: Complete lives on the
// progress record, which eviction keeps, so an evicted job takes the mark, and
// an index outside the progress record's files is refused without a write.
func TestMarkFileComplete_MarksWhenEvicted(t *testing.T) {
	t.Parallel()
	j := New("id", "name", Policy{})
	m := NewManifest([]JobFile{
		{Subject: "a", Bytes: 100, Articles: []JobArticle{{ID: "<a@x>", Bytes: 100, Number: 1}}},
		{Subject: "b", Bytes: 100, Articles: []JobArticle{{ID: "<b@x>", Bytes: 100, Number: 1}}},
	})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	j.Evict()

	if err := j.MarkFileComplete(1); err != nil {
		t.Fatalf("MarkFileComplete on an evicted job: %v", err)
	}
	p := j.Progress()
	if !p.FileComplete(1) {
		t.Error("file 1 is not complete after MarkFileComplete on the evicted job")
	}
	if p.FileComplete(0) {
		t.Error("file 0 is complete; the mark landed on the wrong file")
	}
	for _, fi := range []int{-1, 2} {
		if err := j.MarkFileComplete(fi); err == nil {
			t.Errorf("MarkFileComplete(%d) on a two-file job succeeded, want out of range", fi)
		}
	}
}
