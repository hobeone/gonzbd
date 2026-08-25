package job

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

func TestToSABnzbd(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    StateView
		want constants.Status
	}{
		{"never run", StateView{State: Waiting, Next: Fetching, Reason: NoLease}, constants.StatusQueued},
		{"waiting for a compute slot", StateView{State: Waiting, Next: Assessing, Reason: NoComputeSlot}, constants.StatusQueued},
		{"user paused", StateView{State: Waiting, Next: Fetching, Reason: UserPaused}, constants.StatusPaused},
		{"globally paused", StateView{State: Waiting, Next: Extracting, Reason: GlobalPause}, constants.StatusPaused},

		{"first-pass download", StateView{State: Fetching}, constants.StatusDownloading},
		{"fetching recovery volumes", StateView{State: Fetching, Assessed: true}, constants.StatusFetching},

		{"cheap verification", StateView{State: Assessing, Activity: ActCRCCheck}, constants.StatusQuickCheck},
		{"full verification", StateView{State: Assessing, Activity: ActPar2Verify}, constants.StatusVerifying},
		{"assessing, no activity yet", StateView{State: Assessing}, constants.StatusVerifying},

		{"repairing", StateView{State: Repairing, Activity: ActPar2Repair}, constants.StatusRepairing},
		{"extracting", StateView{State: Extracting, Activity: ActUnpack}, constants.StatusExtracting},
		{"volume recovery is still extracting", StateView{State: Extracting, Activity: ActVolumeRecovery}, constants.StatusExtracting},

		{"finalizing, moving", StateView{State: Finalizing, Activity: ActMove}, constants.StatusMoving},
		{"finalizing, script", StateView{State: Finalizing, Activity: ActScript}, constants.StatusRunning},
		{"finalizing, cleanup", StateView{State: Finalizing, Activity: ActCleanup}, constants.StatusMoving},

		{"completed", StateView{State: Finished, Outcome: OutcomeOK}, constants.StatusCompleted},
		{"failed", StateView{State: Finished, Outcome: OutcomeFailed}, constants.StatusFailed},
		{"unrecoverable renders as failed", StateView{State: Finished, Outcome: OutcomeUnrecoverable}, constants.StatusFailed},
		{"cancelled renders as deleted", StateView{State: Finished, Outcome: OutcomeCancelled}, constants.StatusDeleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToSABnzbd(tc.v); got != tc.want {
				t.Errorf("ToSABnzbd(%+v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}

// TestToSABnzbd_IsTotal walks the whole product space of the axes and fails
// if any combination yields the empty status. The shim is a boundary the API
// depends on, and an unhandled combination there is a blank status string in
// a third-party client rather than a crash — which is exactly the kind of
// failure that ships unnoticed.
func TestToSABnzbd_IsTotal(t *testing.T) {
	states, activities, outcomes, reasons := AllStates(), AllActivities(), AllOutcomes(), AllWaitReasons()
	if len(states) == 0 || len(activities) == 0 || len(outcomes) == 0 || len(reasons) == 0 {
		t.Fatalf("one of the enumeration axes is empty: states=%d activities=%d outcomes=%d reasons=%d",
			len(states), len(activities), len(outcomes), len(reasons))
	}
	for _, s := range states {
		for _, a := range activities {
			for _, o := range outcomes {
				for _, r := range reasons {
					for _, assessed := range []bool{false, true} {
						v := StateView{State: s, Activity: a, Outcome: o, Reason: r, Assessed: assessed}
						if got := ToSABnzbd(v); got == "" {
							t.Errorf("ToSABnzbd(%+v) returned the empty status", v)
						}
					}
				}
			}
		}
	}
}

// TestToSABnzbd_EmitsOnlyDeclaredStatuses guards against a typo producing a
// status string no client knows. Every output must be a declared constant.
func TestToSABnzbd_EmitsOnlyDeclaredStatuses(t *testing.T) {
	declared := make(map[constants.Status]bool)
	all := constants.AllStatuses()
	if len(all) == 0 {
		t.Fatal("constants.AllStatuses() is empty")
	}
	for _, s := range all {
		declared[s] = true
	}
	states, activities, outcomes, reasons := AllStates(), AllActivities(), AllOutcomes(), AllWaitReasons()
	if len(states) == 0 || len(activities) == 0 || len(outcomes) == 0 || len(reasons) == 0 {
		t.Fatalf("one of the enumeration axes is empty: states=%d activities=%d outcomes=%d reasons=%d",
			len(states), len(activities), len(outcomes), len(reasons))
	}
	for _, s := range states {
		for _, a := range activities {
			for _, o := range outcomes {
				for _, r := range reasons {
					for _, assessed := range []bool{false, true} {
						got := ToSABnzbd(StateView{State: s, Activity: a, Outcome: o, Reason: r, Assessed: assessed})
						if !declared[got] {
							t.Errorf("ToSABnzbd emitted %q, which is not in constants.AllStatuses()", got)
						}
					}
				}
			}
		}
	}
}

// TestOnlyOneNonTestFileImportsConstants is the standing check behind
// sabnzbd.go's and doc.go's claim that sabnzbd.go is the only NON-TEST file
// in the package that imports internal/constants. `go list -deps` cannot
// make this check at all — it does not see test files — and a one-time
// manual run of it (as this package's Task 10 review did) is a snapshot,
// not an enforcement point: nothing would catch a second non-test file
// gaining the import later. This test parses the package's own non-test
// sources from scratch on every run and asserts the importer set by name,
// the same shape as scanOutcomeWriters in
// outcome_writer_enumeration_test.go.
func TestOnlyOneNonTestFileImportsConstants(t *testing.T) {
	const wantImporter = "sabnzbd.go"
	const constantsImportPath = `"github.com/hobeone/gonzbd/internal/constants"`

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()
	var importers []string
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, imp := range file.Imports {
			if imp.Path.Value == constantsImportPath {
				importers = append(importers, name)
			}
		}
	}

	// A parse that matched no files would report an empty importers slice,
	// which reads identically to "nobody imports constants" rather than
	// "the scan broke" — the same failure mode scanOutcomeWriters guards
	// against.
	if scanned == 0 {
		t.Fatal("no non-test sources parsed; the scan found nothing to check " +
			"and its empty result would otherwise look like a real answer")
	}

	if len(importers) != 1 || importers[0] != wantImporter {
		t.Errorf("non-test files importing internal/constants = %v, want [%s]\n\n"+
			"sabnzbd.go's and doc.go's comments claim sabnzbd.go is the only "+
			"non-test file that imports internal/constants. If a second file "+
			"needs it, that claim is now false at two comments — update them "+
			"and this list together.", importers, wantImporter)
	}
}

// TestFinishedStatus_MapsEveryOutcome calls finishedStatus directly rather
// than only through ToSABnzbd(State: Finished, ...), since that is the sole
// caller and this pins its own per-Outcome table against a drift in
// ToSABnzbd's routing.
func TestFinishedStatus_MapsEveryOutcome(t *testing.T) {
	cases := []struct {
		o    Outcome
		want constants.Status
	}{
		{OutcomeOK, constants.StatusCompleted},
		{OutcomeCancelled, constants.StatusDeleted},
		{OutcomeFailed, constants.StatusFailed},
		{OutcomeUnrecoverable, constants.StatusFailed},
		{OutcomePending, constants.StatusFailed},
		{Outcome(255), constants.StatusFailed}, // out-of-range: same safe direction as OutcomePending
	}
	for _, c := range cases {
		if got := finishedStatus(c.o); got != c.want {
			t.Errorf("finishedStatus(%v) = %v, want %v", c.o, got, c.want)
		}
	}
}
