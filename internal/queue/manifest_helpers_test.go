package queue

import "testing"

// isPar2File is covered indirectly through the exported API, which is why the
// gap survived; this exercises it head-on so a change is caught at its own
// level rather than through whatever happened to call it.
//
// isPar2File matches on the ".par2" substring anywhere in the subject, which
// is deliberately broader than isPar2Recovery's volume-name convention: this
// one also matches the index file. The two are not interchangeable, and
// counting an index as repair capacity is the bug that distinction exists to
// prevent — so the index case is pinned as a MATCH here on purpose.
func TestIsPar2File(t *testing.T) {
	for _, tc := range []struct {
		subject string
		want    bool
	}{
		{"archive.vol000+01.par2", true},
		{"archive.par2", true},
		{"ARCHIVE.PAR2", true},
		{"Archive.Par2", true},
		{"archive.par2.bin", true},
		{"archive.rar", false},
		{"archive.par3", false},
		{"par2", false},
		{"", false},
	} {
		if got := isPar2File(tc.subject); got != tc.want {
			t.Errorf("isPar2File(%q) = %v, want %v", tc.subject, got, tc.want)
		}
	}
}
