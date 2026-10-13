package postproc

import (
	"log/slog"
	"reflect"
	"testing"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// appliedUnpackStage builds an UnpackStage the way production does: the
// constructor, then Apply.
func appliedUnpackStage(c UnpackConfig) *UnpackStage {
	s := NewUnpackStage()
	s.Apply(c)
	return s
}

// allArchiveTypes allows every archive type, as the config defaults do.
var allArchiveTypes = archiveTypes{rar: true, sevenZip: true, fileJoin: true, tar: true}

// editUnpackConfig re-applies s's settings with edit made to them.
func editUnpackConfig(s *UnpackStage, edit func(*UnpackConfig)) {
	c := s.Config()
	edit(&c)
	s.Apply(c)
}

// appliedRepairStage builds a RepairStage the way production does: the
// constructor, then Apply.
func appliedRepairStage(c RepairConfig, log *slog.Logger) *RepairStage {
	s := NewRepairStage()
	s.Log = log
	s.Apply(c)
	return s
}

// TestStageConfig_ReturnsWhatApplyInstalled pins that Config reports every
// setting Apply installed, so a read through it sees the stage's settings
// rather than a default.
func TestStageConfig_ReturnsWhatApplyInstalled(t *testing.T) {
	t.Parallel()
	uc := UnpackConfig{
		Base:            unpack.Options{UseGoRAR: true, Sandbox: cmdutil.SandboxConfig{Enabled: true, Strict: true}},
		Permissions:     "755",
		PasswordFile:    "/pw.txt",
		EnableUnrar:     true,
		Enable7zip:      true,
		EnableFileJoin:  true,
		EnableTar:       true,
		EnableRecursive: true,
		Cleanup:         true,
	}
	if got := appliedUnpackStage(uc).Config(); !reflect.DeepEqual(got, uc) {
		t.Errorf("UnpackStage.Config() = %+v, want %+v", got, uc)
	}
	rc := RepairConfig{Par2: par2.RunOptions{Command: "/bin/par2", Turbo: true}, UseGoPar2: true, GoPar2Fallback: true}
	if got := appliedRepairStage(rc, nil).Config(); !reflect.DeepEqual(got, rc) {
		t.Errorf("RepairStage.Config() = %+v, want %+v", got, rc)
	}
}
