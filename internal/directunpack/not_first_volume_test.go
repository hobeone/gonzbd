package directunpack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A middle volume named like a first one is not a set DirectUnpack extracted.
// two_members.part2.rar holds only the continuation of second.txt; under the
// name obf.part1.rar it is the whole of the "obf" set. It must fail, not be
// recorded as a success with no files, so the normal unpack stage takes the
// set.
func TestDirectUnpack_MiddleVolumeNamedAsFirstFails(t *testing.T) {
	jobDir := t.TempDir()
	copyRAR(t, testdataDir(t), jobDir, twoMembersVolume(2))
	const name = "obf.part1.rar"
	if err := os.Rename(filepath.Join(jobDir, twoMembersVolume(2)), filepath.Join(jobDir, name)); err != nil {
		t.Fatal(err)
	}

	du := runTwoMembers(t, jobDir, []string{name}, Options{})

	if res, ok := du.Results()["obf"]; ok {
		t.Fatalf("the middle volume was recorded as a successful set: %+v", res)
	}
	failed, ok := du.Failures()["obf"]
	if !ok {
		t.Fatalf("the middle volume did not fail: results %+v, failures %+v", du.Results(), du.Failures())
	}
	if !strings.Contains(failed.Reason, "does not start at the first volume") {
		t.Errorf("failure reason = %q, want it to say the stream does not start at the first volume", failed.Reason)
	}
}
