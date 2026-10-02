package buildinfo

import (
	"runtime/debug"
	"testing"
)

func settings(kv ...string) *debug.BuildInfo {
	bi := &debug.BuildInfo{}
	for i := 0; i < len(kv); i += 2 {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
	}
	return bi
}

const (
	fullSHA = "0123456789abcdef0123456789abcdef01234567"
	vcsTime = "2026-05-01T10:00:00Z"
)

func TestResolve(t *testing.T) {
	vcs := func(modified string) *debug.BuildInfo {
		return settings("vcs.revision", fullSHA, "vcs.time", vcsTime, "vcs.modified", modified)
	}
	tests := []struct {
		name string
		lt   LinkTime
		bi   *debug.BuildInfo
		want Info
	}{
		{
			name: "nothing set and no build info",
			want: Info{Version: DevVersion},
		},
		{
			name: "ldflags only, nil build info",
			lt:   LinkTime{"v1.2.3", "abc1234", "2026-04-01T00:00:00Z", "true", "2026-05-06T14:00:00Z"},
			want: Info{"v1.2.3", "abc1234", "2026-04-01T00:00:00Z", true, "2026-05-06T14:00:00Z"},
		},
		{
			name: "plain go build falls back to vcs settings",
			bi:   vcs("false"),
			want: Info{Version: DevVersion, Commit: "0123456", CommitTime: vcsTime},
		},
		{
			name: "vcs.modified true sets Dirty",
			bi:   vcs("true"),
			want: Info{Version: DevVersion, Commit: "0123456", CommitTime: vcsTime, Dirty: true},
		},
		{
			name: "ldflags commit wins over vcs.revision; time and dirty fall back to vcs",
			lt:   LinkTime{Version: "v1", Commit: "0123456789", BuildDate: "2026-05-06T14:00:00Z"},
			bi:   vcs("true"),
			want: Info{"v1", "0123456789", vcsTime, true, "2026-05-06T14:00:00Z"},
		},
		{
			name: "ldflags commit time wins over vcs.time",
			lt:   LinkTime{Commit: "0123456", CommitTime: "2026-04-01T00:00:00Z"},
			bi:   vcs("false"),
			want: Info{Version: DevVersion, Commit: "0123456", CommitTime: "2026-04-01T00:00:00Z"},
		},
		{
			name: "ldflags dirty false wins over vcs.modified true",
			lt:   LinkTime{Commit: "0123456", Dirty: "false"},
			bi:   vcs("true"),
			want: Info{Version: DevVersion, Commit: "0123456", CommitTime: vcsTime},
		},
		{
			name: "ldflags dirty true wins over vcs.modified false",
			lt:   LinkTime{Commit: "0123456", Dirty: "true"},
			bi:   vcs("false"),
			want: Info{Version: DevVersion, Commit: "0123456", CommitTime: vcsTime, Dirty: true},
		},
		{
			name: "unparseable ldflags dirty counts as unset",
			lt:   LinkTime{Commit: "0123456", Dirty: "maybe"},
			bi:   vcs("true"),
			want: Info{Version: DevVersion, Commit: "0123456", CommitTime: vcsTime, Dirty: true},
		},
		{
			name: "vcs settings for a different commit are ignored",
			lt:   LinkTime{Version: "v1", Commit: "fedcba9"},
			bi:   vcs("true"),
			want: Info{Version: "v1", Commit: "fedcba9"},
		},
		{
			name: "short vcs.revision is not padded or truncated",
			bi:   settings("vcs.revision", "abc"),
			want: Info{Version: DevVersion, Commit: "abc"},
		},
		{
			name: "build info without vcs settings leaves everything absent",
			bi:   settings("-compiler", "gc"),
			want: Info{Version: DevVersion},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Resolve(tt.lt, tt.bi); got != tt.want {
				t.Errorf("Resolve() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLabels(t *testing.T) {
	tests := []struct {
		name       string
		info       Info
		commit     string
		buildLabel string
	}{
		{"absent", Info{}, "unknown", "unknown"},
		{"clean", Info{Commit: "abc1234", BuildDate: "2026-05-06T14:00:00Z"}, "abc1234", "2026-05-06T14:00:00Z"},
		{"dirty", Info{Commit: "abc1234", Dirty: true}, "abc1234 (modified)", "unknown"},
		{"dirty without commit", Info{Dirty: true}, "unknown", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.info.CommitLabel(); got != tt.commit {
				t.Errorf("CommitLabel() = %q, want %q", got, tt.commit)
			}
			if got := tt.info.BuildDateLabel(); got != tt.buildLabel {
				t.Errorf("BuildDateLabel() = %q, want %q", got, tt.buildLabel)
			}
		})
	}
}
