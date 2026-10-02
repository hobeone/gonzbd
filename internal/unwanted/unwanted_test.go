package unwanted

import (
	"slices"
	"testing"
)

var defaultList = []string{"exe", "com", "scr", "pif", "bat", "cmd", "msi", "vbs"}

func mustRules(t *testing.T, action Action, mode Mode, exts []string) Rules {
	t.Helper()
	r, err := NewRules(action, mode, exts)
	if err != nil {
		t.Fatalf("NewRules(%q, %q, %q): %v", action, mode, exts, err)
	}
	return r
}

func TestUnwanted_Blacklist(t *testing.T) {
	t.Parallel()
	r := mustRules(t, ActionPause, ModeBlacklist, defaultList)
	cases := []struct {
		name string
		want bool
	}{
		{"setup.exe", true},
		{"movie.mkv", false},
		{"SETUP.EXE", true},
		{"Setup.Exe", true},
		{"setup.exe.", true},
		{"setup.exe...", true},
		{"setup.exe ", true},
		{"setup.exe . .", true},
		{"setup.exe\t", true},
		{"setup.exe\x00", true},
		{"setup.exe ", true},
		{"dir/setup.exe", true},
		{`dir\setup.exe`, true},
		{"x.exe/readme.txt", false},
		{`x.exe\readme.txt`, false},
		{"setup.exe.txt", false},
		{"readme.txt.exe", true},
		{".exe", true},
		{"exe", false},
		{"noextension", false},
		{"", false},
		{"trailingdot.", false},
		{"archive.rar", false},
		{"payload.scr", true},
		{"script.vbs", true},
		{"setup.ex", false},
		{"setup.exee", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := r.Unwanted(c.name); got != c.want {
				t.Errorf("Unwanted(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestUnwanted_Whitelist(t *testing.T) {
	t.Parallel()
	r := mustRules(t, ActionPause, ModeWhitelist, []string{"mkv", "srt", "nfo"})
	cases := []struct {
		name string
		want bool
	}{
		{"movie.mkv", false},
		{"MOVIE.MKV", false},
		{"movie.srt", false},
		{"setup.exe", true},
		{"movie.mkv.exe", true},
		{"setup.exe.mkv", false},
		// No extension is never unwanted, or every obfuscated post would be.
		{"a8f3c1d2e4b5a6c7d8e9f0a1b2c3d4e5", false},
		{"", false},
		{"movie.mkv. ", false},
		// The only dot is in a directory, so the name has no extension.
		{"release.v1/readme", false},
		{`release.v1\readme`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := r.Unwanted(c.name); got != c.want {
				t.Errorf("Unwanted(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestUnwanted_EmptyList(t *testing.T) {
	t.Parallel()
	black := mustRules(t, ActionPause, ModeBlacklist, nil)
	if black.Unwanted("setup.exe") {
		t.Error("blacklist with no entries reported setup.exe unwanted")
	}
	white := mustRules(t, ActionPause, ModeWhitelist, nil)
	if !white.Unwanted("movie.mkv") {
		t.Error("whitelist with no entries let movie.mkv through")
	}
	if white.Unwanted("noextension") {
		t.Error("whitelist with no entries reported a name with no extension unwanted")
	}
}

func TestNewRules_NormalisesEntries(t *testing.T) {
	t.Parallel()
	r := mustRules(t, ActionFail, ModeBlacklist, []string{" .EXE ", "..Scr", "", "   ", "r[0-9][0-9]"})
	for _, name := range []string{"a.exe", "a.scr", "a.r01"} {
		if !r.Unwanted(name) {
			t.Errorf("Unwanted(%q) = false after normalisation, want true", name)
		}
	}
	if r.Unwanted("a.r001") {
		t.Error(`pattern "r[0-9][0-9]" matched "r001"; a pattern must match the whole extension`)
	}
	if got := len(r.patterns); got != 3 {
		t.Errorf("kept %d patterns, want 3 (blank entries dropped): %q", got, r.patterns)
	}
}

func TestNewRules_Rejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		action Action
		mode   Mode
		exts   []string
	}{
		{"bad action", "abort", ModeBlacklist, nil},
		{"empty action", "", ModeBlacklist, nil},
		{"bad mode", ActionPause, "allowlist", nil},
		{"empty mode", ActionPause, "", nil},
		{"bad pattern", ActionPause, ModeBlacklist, []string{"exe", "[x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewRules(c.action, c.mode, c.exts); err == nil {
				t.Errorf("NewRules(%q, %q, %q) = nil error, want one", c.action, c.mode, c.exts)
			}
		})
	}
}

// TestListed_FailsClosedOnAPatternError pins listed's error arm. NewRules
// refuses a malformed pattern, so this builds Rules directly: the arm is
// what decides a name if a pattern ever reaches matching unvalidated, and it
// must decide "unwanted".
func TestListed_FailsClosedOnAPatternError(t *testing.T) {
	t.Parallel()
	r := Rules{action: ActionPause, mode: ModeBlacklist, patterns: []string{"[x"}}
	if !r.Unwanted("setup.zzz") {
		t.Error("a malformed pattern let the name through; the check must fail closed")
	}
}

func TestFind(t *testing.T) {
	t.Parallel()
	r := mustRules(t, ActionPause, ModeBlacklist, defaultList)
	got := r.Find([]string{"a.mkv", "b.exe", "c.nfo", "d.SCR"})
	if want := []string{"b.exe", "d.SCR"}; !slices.Equal(got, want) {
		t.Errorf("Find = %q, want %q", got, want)
	}
	if got := r.Find([]string{"a.mkv"}); got != nil {
		t.Errorf("Find with nothing unwanted = %q, want nil", got)
	}
}

func TestRules_ZeroValueIsOff(t *testing.T) {
	t.Parallel()
	var r Rules
	if r.Action() != ActionOff {
		t.Errorf("zero Rules Action() = %q, want %q", r.Action(), ActionOff)
	}
	if got := mustRules(t, ActionFail, ModeBlacklist, nil).Action(); got != ActionFail {
		t.Errorf("Action() = %q, want %q", got, ActionFail)
	}
}

func TestEnumValid(t *testing.T) {
	t.Parallel()
	for _, m := range []Mode{ModeBlacklist, ModeWhitelist} {
		if !m.Valid() {
			t.Errorf("Mode %q not Valid", m)
		}
	}
	if Mode("x").Valid() {
		t.Error(`Mode "x" Valid`)
	}
	for _, a := range []Action{ActionOff, ActionPause, ActionFail} {
		if !a.Valid() {
			t.Errorf("Action %q not Valid", a)
		}
	}
	if Action("x").Valid() {
		t.Error(`Action "x" Valid`)
	}
	for _, s := range []State{StateNone, StateBlocked, StateApproved} {
		if !s.Valid() {
			t.Errorf("State %d not Valid", s)
		}
	}
	if State(3).Valid() {
		t.Error("State 3 Valid")
	}
}
