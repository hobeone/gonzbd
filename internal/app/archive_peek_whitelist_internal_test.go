package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/unwanted"
)

// The peek must judge the population unwanted_cleanup judges. In whitelist
// mode every name not listed is unwanted, so a par2 file's declared archive
// volumes and par2 files, which the pipeline consumes before that stage, must
// not block a job the backstop would let through.
func TestPeek_WhitelistPar2ContainerNamesAreNotJudged(t *testing.T) {
	t.Parallel()
	a := newPeekAppMode(t, unwanted.ActionPause, unwanted.ModeWhitelist, []string{"mkv", "nfo", "srt"}, false,
		[]peekFile{{"release.par2", par2Declaring("movie.part01.rar", "movie.r00", "movie.rar", "movie.7z.001", "movie.par2", "movie.vol000+01.par2", "movie.mkv", "movie.nfo")}})
	a.complete(t, 0)
	if got := a.state(t); got != unwanted.StateNone {
		t.Fatalf("Unwanted = %d, want none: the peek judged names the pipeline consumes", got)
	}
}

func TestPeek_WhitelistPar2StillBlocksAnUnlistedName(t *testing.T) {
	t.Parallel()
	a := newPeekAppMode(t, unwanted.ActionPause, unwanted.ModeWhitelist, []string{"mkv", "nfo", "srt"}, false,
		[]peekFile{{"release.par2", par2Declaring("movie.part01.rar", "movie.mkv", "payload.exe")}})
	a.complete(t, 0)
	if got := a.state(t); got != unwanted.StateBlocked {
		t.Fatalf("Unwanted = %d, want blocked by payload.exe", got)
	}
}

// A name that merely contains ".par2" is not a par2 file the pipeline
// consumes, so the backstop judges it; the peek must too. The exemption is
// the par2 extension, not a substring.
func TestPeek_Par2LookalikeNamesAreJudged(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"evil.par2.exe", "evil.PAR2x.scr", "evil.par2 .exe"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := newPeekAppMode(t, unwanted.ActionPause, unwanted.ModeBlacklist, []string{"exe", "scr"}, false,
				[]peekFile{{"release.par2", par2Declaring("movie.mkv", name)}})
			a.complete(t, 0)
			if got := a.state(t); got != unwanted.StateBlocked {
				t.Fatalf("Unwanted = %d, want blocked by %q", got, name)
			}
		})
	}
}

// The real par2 and volume names stay exempt in whitelist mode, where every
// unlisted extension is unwanted.
func TestPeek_WhitelistRealPar2AndVolumeNamesStayExempt(t *testing.T) {
	t.Parallel()
	a := newPeekAppMode(t, unwanted.ActionPause, unwanted.ModeWhitelist, []string{"mkv"}, false,
		[]peekFile{{"release.par2", par2Declaring("movie.vol01+02.par2", "movie.PAR2", "movie.part01.rar", "movie.mkv")}})
	a.complete(t, 0)
	if got := a.state(t); got != unwanted.StateNone {
		t.Fatalf("Unwanted = %d, want none", got)
	}
}

func TestPeek_WhitelistRARMembers(t *testing.T) {
	t.Parallel()
	rar := unpackFixture(t, "single_rar5.rar") // file1.txt, file2.txt, nested.txt
	for _, c := range []struct {
		name string
		list []string
		want unwanted.State
	}{
		{"every member listed", []string{"txt"}, unwanted.StateNone},
		{"members not listed", []string{"mkv"}, unwanted.StateBlocked},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a := newPeekAppMode(t, unwanted.ActionPause, unwanted.ModeWhitelist, c.list, false,
				[]peekFile{{"x.rar", rar}})
			a.complete(t, 0)
			if got := a.state(t); got != c.want {
				t.Errorf("Unwanted = %d, want %d", got, c.want)
			}
		})
	}
}
