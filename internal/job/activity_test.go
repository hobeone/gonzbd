package job

import (
	"strconv"
	"testing"
)

func TestActivity_String(t *testing.T) {
	for _, tc := range []struct {
		a    Activity
		want string
	}{
		{ActNone, "None"},
		{ActCRCCheck, "CRCCheck"},
		{ActPar2Verify, "Par2Verify"},
		{ActPar2Repair, "Par2Repair"},
		{ActVolumeRecovery, "VolumeRecovery"},
		{ActUnpack, "Unpack"},
		{ActDeobfuscate, "Deobfuscate"},
		{ActCleanup, "Cleanup"},
		{ActMove, "Move"},
		{ActScript, "Script"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.a.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
	if got := Activity(77).String(); got != "Activity(77)" {
		t.Errorf("String() = %q, want %q", got, "Activity(77)")
	}
}

func TestActivity_NoneIsZero(t *testing.T) {
	var a Activity
	if a != ActNone {
		t.Errorf("zero Activity = %v, want ActNone; StateView's zero value depends on this", a)
	}
}

func TestAllActivities_EveryEntryHasAStringArm(t *testing.T) {
	for _, a := range AllActivities() {
		if got := a.String(); got == "Activity("+strconv.Itoa(int(a))+")" {
			t.Errorf("Activity(%d) is in AllActivities() but falls to the default String() arm", a)
		}
	}
}
