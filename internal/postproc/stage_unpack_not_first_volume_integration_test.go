//go:build integration

package postproc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/unpack"
)

// The fallback skip against a real unrar. late_member.part3..6 renamed
// obf.part1..4 is a set missing its first two volumes: first.bin only
// continues in it, and second.bin starts in it and is complete. unrar 7.12
// given that set writes all of second.bin, 268 MB, before exiting with an
// error for first.bin. With GoRarFallback on and unrar on PATH, the unpack
// stage must not run it: go_unrar's failure is the one reported, nothing is
// extracted, and every volume stays where it is.
func TestUnpackStage_MissingFirstVolumesIsNotHandedToUnrar(t *testing.T) {
	if _, err := exec.LookPath("unrar"); err != nil {
		t.Skip("unrar not installed")
	}
	job, dir := stageJob(t)
	var mains []string
	for i := 3; i <= 6; i++ {
		src := copyToDir(t, unpackFixture(fmt.Sprintf("late_member.part%d.rar", i)), dir)
		dst := filepath.Join(dir, fmt.Sprintf("obf.part%d.rar", i-2))
		if err := os.Rename(src, dst); err != nil {
			t.Fatal(err)
		}
		mains = append(mains, dst)
	}

	s := appliedUnpackStage(UnpackConfig{
		Base:        unpack.Options{UseGoRAR: true, GoRarFallback: true},
		EnableUnrar: true, Enable7zip: true, Cleanup: true,
	})
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err == nil {
		t.Errorf("Run = nil, want the set missing its first volumes to fail (output %q)", job.OutputLines)
	}
	if !job.UnpackError {
		t.Error("UnpackError = false, want true")
	}
	if !slices.Contains(job.OutputLines, "[go_unrar] obf (FAILED)") ||
		slices.ContainsFunc(job.OutputLines, func(l string) bool { return strings.Contains(l, "retrying with external") }) {
		t.Errorf("output = %q, want go_unrar's failure reported and no external retry", job.OutputLines)
	}
	for _, m := range mains {
		if job.isPendingDeletion(m) {
			t.Errorf("%s was queued for deletion", filepath.Base(m))
		}
	}
	for _, name := range []string{"first.bin", "second.bin"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s exists in the job dir (%v), want nothing extracted", name, err)
		}
	}
}
