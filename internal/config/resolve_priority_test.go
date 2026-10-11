package config

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// TestResolvePriority covers each step of the inheritance chain: an explicit
// request, the category, the fallback category, and Normal.
func TestResolvePriority(t *testing.T) {
	t.Parallel()
	cats := func(tv, def int) []CategoryConfig {
		return []CategoryConfig{{Name: "Default", Priority: def}, {Name: "tv", Priority: tv}}
	}
	for _, tc := range []struct {
		name string
		cats []CategoryConfig
		req  constants.Priority
		want constants.Priority
	}{
		{"explicit request is kept", cats(int(constants.HighPriority), 0), constants.LowPriority, constants.LowPriority},
		{"category priority is inherited", cats(int(constants.HighPriority), 0), constants.DefaultPriority, constants.HighPriority},
		{"category Paused is inherited", cats(int(constants.PausedPriority), 0), constants.DefaultPriority, constants.PausedPriority},
		{"category Default defers to the fallback", cats(int(constants.DefaultPriority), int(constants.ForcePriority)), constants.DefaultPriority, constants.ForcePriority},
		{"category Stop defers to the fallback", cats(int(constants.StopPriority), int(constants.LowPriority)), constants.DefaultPriority, constants.LowPriority},
		{"unknown category value defers to the fallback", cats(7, int(constants.HighPriority)), constants.DefaultPriority, constants.HighPriority},
		// 257 truncates to int8 1, High: only the range check keeps it from
		// passing as a concrete priority.
		{"out-of-range category value defers to the fallback", cats(257, int(constants.LowPriority)), constants.DefaultPriority, constants.LowPriority},
		{"both sentinels give Normal", cats(int(constants.DefaultPriority), int(constants.DefaultPriority)), constants.DefaultPriority, constants.NormalPriority},
		{"no categories gives the builtin Normal", nil, constants.DefaultPriority, constants.NormalPriority},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ResolvePriority(tc.cats, "tv", tc.req); got != tc.want {
				t.Errorf("ResolvePriority = %s, want %s", got, tc.want)
			}
		})
	}
}
