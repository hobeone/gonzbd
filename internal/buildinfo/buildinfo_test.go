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

const fullSHA = "0123456789abcdef0123456789abcdef01234567"

func TestResolve(t *testing.T) {
	tests := []struct {
		name                  string
		version, commit, date string
		bi                    *debug.BuildInfo
		want                  Info
	}{
		{
			name: "nothing set and no build info",
			want: Info{Version: DevVersion, Commit: Unknown, BuildDate: Unknown},
		},
		{
			name:    "ldflags only, nil build info",
			version: "v1.2.3", commit: "abc1234", date: "2026-05-06T14:00:00Z",
			want: Info{Version: "v1.2.3", Commit: "abc1234", BuildDate: "2026-05-06T14:00:00Z"},
		},
		{
			name:    "plain go build falls back to vcs settings",
			version: DevVersion, commit: Unknown, date: Unknown,
			bi: settings("vcs.revision", fullSHA, "vcs.time", "2026-05-01T10:00:00Z", "vcs.modified", "false"),
			want: Info{
				Version: DevVersion, Commit: "0123456", CommitTime: "2026-05-01T10:00:00Z",
				BuildDate: Unknown,
			},
		},
		{
			name:    "vcs.modified true sets Dirty",
			version: DevVersion, commit: Unknown, date: Unknown,
			bi: settings("vcs.revision", fullSHA, "vcs.time", "2026-05-01T10:00:00Z", "vcs.modified", "true"),
			want: Info{
				Version: DevVersion, Commit: "0123456", CommitTime: "2026-05-01T10:00:00Z",
				Dirty: true, BuildDate: Unknown,
			},
		},
		{
			name: "empty ldflags strings count as unset",
			bi:   settings("vcs.revision", fullSHA),
			want: Info{Version: DevVersion, Commit: "0123456", BuildDate: Unknown},
		},
		{
			name:    "ldflags commit wins over vcs.revision; time and dirty still come from vcs",
			version: "v1.0.0", commit: "0123456789", date: "2026-05-06T14:00:00Z",
			bi: settings("vcs.revision", fullSHA, "vcs.time", "2026-05-01T10:00:00Z", "vcs.modified", "true"),
			want: Info{
				Version: "v1.0.0", Commit: "0123456789", CommitTime: "2026-05-01T10:00:00Z",
				Dirty: true, BuildDate: "2026-05-06T14:00:00Z",
			},
		},
		{
			name:    "vcs settings for a different commit are ignored",
			version: "v1.0.0", commit: "fedcba9", date: "2026-05-06T14:00:00Z",
			bi:   settings("vcs.revision", fullSHA, "vcs.time", "2026-05-01T10:00:00Z", "vcs.modified", "true"),
			want: Info{Version: "v1.0.0", Commit: "fedcba9", BuildDate: "2026-05-06T14:00:00Z"},
		},
		{
			name:    "short vcs.revision is not padded or truncated",
			version: DevVersion, commit: Unknown, date: Unknown,
			bi:   settings("vcs.revision", "abc"),
			want: Info{Version: DevVersion, Commit: "abc", BuildDate: Unknown},
		},
		{
			name:    "build info without vcs settings leaves everything unknown",
			version: DevVersion, commit: Unknown, date: Unknown,
			bi:   settings("-compiler", "gc"),
			want: Info{Version: DevVersion, Commit: Unknown, BuildDate: Unknown},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Resolve(tt.version, tt.commit, tt.date, tt.bi); got != tt.want {
				t.Errorf("Resolve() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
