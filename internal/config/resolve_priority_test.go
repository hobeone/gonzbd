package config

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// TestResolvePriority covers each step of the inheritance chain: an explicit
// request, the category, the fallback category, and Normal; and the Paused
// request and category priority, which pause the job and hold what the chain
// finds past them.
func TestResolvePriority(t *testing.T) {
	t.Parallel()
	cats := func(tv, def int) []CategoryConfig {
		return []CategoryConfig{{Name: "Default", Priority: def}, {Name: "tv", Priority: tv}}
	}
	for _, tc := range []struct {
		name      string
		cats      []CategoryConfig
		req       constants.Priority
		want      constants.Priority
		wantPause bool
	}{
		{"explicit request is kept", cats(int(constants.HighPriority), 0), constants.LowPriority, constants.LowPriority, false},
		{"explicit Repair is kept", cats(int(constants.HighPriority), 0), constants.RepairPriority, constants.RepairPriority, false},
		{"category priority is inherited", cats(int(constants.HighPriority), 0), constants.DefaultPriority, constants.HighPriority, false},
		{"category Paused pauses and defers to the fallback", cats(int(constants.PausedPriority), int(constants.LowPriority)), constants.DefaultPriority, constants.LowPriority, true},
		{"fallback Paused pauses and gives Normal", cats(int(constants.DefaultPriority), int(constants.PausedPriority)), constants.DefaultPriority, constants.NormalPriority, true},
		{"category Default defers to the fallback", cats(int(constants.DefaultPriority), int(constants.ForcePriority)), constants.DefaultPriority, constants.ForcePriority, false},
		{"category Stop defers to the fallback", cats(int(constants.StopPriority), int(constants.LowPriority)), constants.DefaultPriority, constants.LowPriority, false},
		{"unknown category value defers to the fallback", cats(7, int(constants.HighPriority)), constants.DefaultPriority, constants.HighPriority, false},
		// 257 truncates to int8 1, High: only the range check keeps it from
		// passing as a concrete priority.
		{"out-of-range category value defers to the fallback", cats(257, int(constants.LowPriority)), constants.DefaultPriority, constants.LowPriority, false},
		{"both sentinels give Normal", cats(int(constants.DefaultPriority), int(constants.DefaultPriority)), constants.DefaultPriority, constants.NormalPriority, false},
		{"no categories gives the builtin Normal", nil, constants.DefaultPriority, constants.NormalPriority, false},
		{"Paused request pauses and holds the category's", cats(int(constants.HighPriority), int(constants.LowPriority)), constants.PausedPriority, constants.HighPriority, true},
		{"Paused request under a Paused category holds the fallback's", cats(int(constants.PausedPriority), int(constants.LowPriority)), constants.PausedPriority, constants.LowPriority, true},
		{"Stop is not resolved", cats(int(constants.HighPriority), 0), constants.StopPriority, constants.StopPriority, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, pause := ResolvePriority(tc.cats, "tv", tc.req)
			if got != tc.want || pause != tc.wantPause {
				t.Errorf("ResolvePriority = (%s, %v), want (%s, %v)", got, pause, tc.want, tc.wantPause)
			}
		})
	}
}
