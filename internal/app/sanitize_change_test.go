package app

import (
	"os"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// TestRecordedFileName_SurvivesASanitizeChange pins that a file's recorded
// name is where both the verifier and a later registration look for it, after
// the sanitize options change (#905). A file named under the defaults keeps
// its spaces and diacritics; re-sanitizing the recorded name under
// replace_spaces_with and strip_diacritics would look for a different file,
// which the verifier reads as missing and refetches.
func TestRecordedFileName_SurvivesASanitizeChange(t *testing.T) {
	t.Parallel()
	const subject = "My Show Café.bin"
	application, j := newDurabilityTestAppSubjects(t, []string{subject}, 1)
	p := application.pipeline

	writeFixtureArticle(t, application, j.ID(), 0, 0)
	info, err := p.resolveFileInfo(j.ID(), 0)
	if err != nil {
		t.Fatalf("resolveFileInfo: %v", err)
	}
	written := info.Path()
	if info.Name != subject {
		t.Fatalf("writer named the file %q, want %q under the default options", info.Name, subject)
	}
	if _, err := os.Stat(written); err != nil {
		t.Fatalf("the writer's file: %v", err)
	}
	_, recorded := j.FileLocation(0)

	// The options a restart (or a settings change) hands the pipeline.
	p.mu.Lock()
	p.sanitize = fsutil.SanitizeOptions{ReplaceSpacesWith: "_", StripDiacritics: true}
	p.mu.Unlock()

	t.Run("verifier", func(t *testing.T) {
		loc, err := application.residency.locate(j.Name(), recorded)
		if err != nil {
			t.Fatalf("locate(%q): %v", recorded, err)
		}
		if loc.Path() != written {
			t.Errorf("verifier reads %q, want the writer's %q", loc.Path(), written)
		}
	})
	t.Run("re-registration", func(t *testing.T) {
		p.forgetFile(j.ID(), 0)
		if err := p.registerFile(j, 0); err != nil {
			t.Fatalf("registerFile: %v", err)
		}
		again, err := p.resolveFileInfo(j.ID(), 0)
		if err != nil {
			t.Fatalf("resolveFileInfo: %v", err)
		}
		if again.Path() != written {
			t.Errorf("re-registered at %q, want the writer's %q", again.Path(), written)
		}
	})
}
