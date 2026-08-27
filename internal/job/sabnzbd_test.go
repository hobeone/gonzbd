package job

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

func TestToSABnzbd(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    RenderView
		want constants.Status
	}{
		{"never run", RenderView{State: StateUnset}, constants.StatusQueued},
		{"never run, paused", RenderView{State: StateUnset, Reason: UserPaused, Intent: IntentPause}, constants.StatusPaused},

		{"waiting for a lease", RenderView{State: Fetching, Reason: NoLease}, constants.StatusQueued},
		{"waiting for a compute slot", RenderView{State: Fetching, Reason: NoComputeSlot}, constants.StatusQueued},
		{"user paused", RenderView{State: Fetching, Reason: UserPaused, Intent: IntentPause}, constants.StatusPaused},
		{"globally paused", RenderView{State: Extracting, Reason: GlobalPause, Intent: IntentRun}, constants.StatusPaused},

		{"first-pass download", RenderView{State: Fetching, Running: true}, constants.StatusDownloading},
		{"fetching recovery volumes", RenderView{State: Fetching, Assessed: true, Running: true}, constants.StatusFetching},

		{"cheap verification", RenderView{State: Assessing, Activity: ActCRCCheck, Running: true}, constants.StatusQuickCheck},
		{"full verification", RenderView{State: Assessing, Activity: ActPar2Verify, Running: true}, constants.StatusVerifying},
		{"assessing, no activity yet", RenderView{State: Assessing, Running: true}, constants.StatusVerifying},

		{"repairing", RenderView{State: Repairing, Activity: ActPar2Repair, Running: true}, constants.StatusRepairing},
		{"extracting", RenderView{State: Extracting, Activity: ActUnpack, Running: true}, constants.StatusExtracting},
		{"volume recovery is still extracting", RenderView{State: Extracting, Activity: ActVolumeRecovery, Running: true}, constants.StatusExtracting},

		{"finalizing, moving", RenderView{State: Finalizing, Activity: ActMove, Running: true}, constants.StatusMoving},
		{"finalizing, script", RenderView{State: Finalizing, Activity: ActScript, Running: true}, constants.StatusRunning},
		{"finalizing, cleanup", RenderView{State: Finalizing, Activity: ActCleanup, Running: true}, constants.StatusMoving},

		// A RUNNING job with IntentPause renders as its state, not Paused. It
		// is still repairing; the pause takes effect at the next gate. This is
		// the whole point of the axis — see design §1.1.
		{"running, pause requested", RenderView{State: Repairing, Activity: ActPar2Repair, Running: true, Intent: IntentPause}, constants.StatusRepairing},
		{"running, cancel requested", RenderView{State: Extracting, Activity: ActUnpack, Running: true, Intent: IntentCancel}, constants.StatusExtracting},

		// Settled rows, each carrying a DIFFERENT position, because
		// settledness is now read off Outcome and the position is whatever the
		// attempt happened to reach. A single shared state here would have
		// hidden the change: these four would still pass if ToSABnzbd secretly
		// keyed on that one state. Each is also a position the outcome can
		// really settle from — OutcomeOK only past the boundary,
		// OutcomeUnrecoverable only before it (see finish).
		{"completed", RenderView{State: Finalizing, Outcome: OutcomeOK}, constants.StatusCompleted},
		{"failed while fetching", RenderView{State: Fetching, Outcome: OutcomeFailed}, constants.StatusFailed},
		{"unrecoverable renders as failed", RenderView{State: Assessing, Outcome: OutcomeUnrecoverable}, constants.StatusFailed},
		{"cancelled renders as deleted", RenderView{State: Repairing, Outcome: OutcomeCancelled}, constants.StatusDeleted},
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
	for _, s := range append(states, StateUnset) {
		for _, a := range activities {
			for _, o := range outcomes {
				for _, r := range reasons {
					for _, in := range mustAllIntents(t) {
						for _, assessed := range []bool{false, true} {
							for _, running := range []bool{false, true} {
								v := RenderView{
									State: s, Activity: a, Outcome: o, Assessed: assessed,
									Running: running, Reason: r, Intent: in,
								}
								if got := ToSABnzbd(v); got == "" {
									t.Errorf("ToSABnzbd(%+v) returned the empty status", v)
								}
							}
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
	for _, s := range append(states, StateUnset) {
		for _, a := range activities {
			for _, o := range outcomes {
				for _, r := range reasons {
					for _, in := range mustAllIntents(t) {
						for _, assessed := range []bool{false, true} {
							for _, running := range []bool{false, true} {
								v := RenderView{
									State: s, Activity: a, Outcome: o, Assessed: assessed,
									Running: running, Reason: r, Intent: in,
								}
								got := ToSABnzbd(v)
								if !declared[got] {
									t.Errorf("ToSABnzbd emitted %q, which is not in constants.AllStatuses()", got)
								}
							}
						}
					}
				}
			}
		}
	}
}

// TestToSABnzbd_NeverEmitsUnproducedStatuses is what sabnzbd.go's doc
// comment on ToSABnzbd actually needs cited: it walks the same product space
// as TestToSABnzbd_IsTotal and TestToSABnzbd_EmitsOnlyDeclaredStatuses and
// asserts none of the four upstream statuses this design has no analogue for
// — Idle, Grabbing, Propagating, Checking — ever comes out of ToSABnzbd.
// TestToSABnzbd_EmitsOnlyDeclaredStatuses cannot catch this: it checks
// membership in constants.AllStatuses(), which contains all four, so a
// ToSABnzbd branch that started returning one of them would pass that test
// silently. Guarded against a vacuous pass the same way IsTotal is: an empty
// enumeration axis fails loudly instead of the loop running zero times.
func TestToSABnzbd_NeverEmitsUnproducedStatuses(t *testing.T) {
	unproduced := map[constants.Status]bool{
		constants.StatusIdle:        true,
		constants.StatusGrabbing:    true,
		constants.StatusPropagating: true,
		constants.StatusChecking:    true,
	}
	states, activities, outcomes, reasons := AllStates(), AllActivities(), AllOutcomes(), AllWaitReasons()
	if len(states) == 0 || len(activities) == 0 || len(outcomes) == 0 || len(reasons) == 0 {
		t.Fatalf("one of the enumeration axes is empty: states=%d activities=%d outcomes=%d reasons=%d",
			len(states), len(activities), len(outcomes), len(reasons))
	}
	for _, s := range append(states, StateUnset) {
		for _, a := range activities {
			for _, o := range outcomes {
				for _, r := range reasons {
					for _, in := range mustAllIntents(t) {
						for _, assessed := range []bool{false, true} {
							for _, running := range []bool{false, true} {
								v := RenderView{
									State: s, Activity: a, Outcome: o, Assessed: assessed,
									Running: running, Reason: r, Intent: in,
								}
								got := ToSABnzbd(v)
								if unproduced[got] {
									t.Errorf("ToSABnzbd(%+v) = %q, which this design has no analogue for and should never produce",
										v, got)
								}
							}
						}
					}
				}
			}
		}
	}
}

// TestToSABnzbd_GlobalPauseRendersAsPaused is the regression pin for the one
// finding in this area that reached a shipped revision of the design: a table
// keyed on Intent renders a globally-paused queue as Queued, because each job
// still carries IntentRun. Keyed on WaitReason.IsPause() it cannot.
func TestToSABnzbd_GlobalPauseRendersAsPaused(t *testing.T) {
	// Every state, with no exception to carve out: settling is an Outcome
	// fact, so a zero Outcome here means every position below is genuinely
	// unsettled and genuinely waiting.
	for _, s := range mustAllStates(t) {
		v := RenderView{State: s, Running: false, Reason: GlobalPause, Intent: IntentRun}
		if got := ToSABnzbd(v); got != constants.StatusPaused {
			t.Errorf("ToSABnzbd(%+v) = %q, want StatusPaused; a queue-wide pause leaves every job at IntentRun, "+
				"so a table keyed on Intent would report the whole queue as Queued", v, got)
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
	const constantsImportPath = "github.com/hobeone/gonzbd/internal/constants"

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
			// imp.Path.Value is the raw string literal, quotes and all
			// (`"github.com/..."`), and Go also permits a backtick-quoted
			// import path — strconv.Unquote handles both, where a bare `==`
			// against a double-quoted literal would silently pass a
			// backtick-quoted import straight through the check.
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import path %s: %v", name, imp.Path.Value, err)
			}
			if path == constantsImportPath {
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
// than only through ToSABnzbd on a settled view, since ToSABnzbd is its
// sole non-test caller — `git grep -n 'finished[S]tatus(' -- 'internal/job/*.go'
// ':!internal/job/*_test.go'` returns exactly two lines, the definition at
// sabnzbd.go:103 and ToSABnzbd's call at sabnzbd.go:47. This pins its own
// per-Outcome table against a drift in ToSABnzbd's routing.
//
// TestFinishedStatus_HasOneNonTestCaller enforces that population rather than
// leaving it to a reader who runs the grep: the citation says what is true
// today, the test is what fails when it stops being.
//
// The bracketed S and the _test.go exclusion are both load-bearing. Without
// the brackets this citation matches its own text; without the exclusion it
// also returns this test's call and its error-format string, and a reader
// checking the claim has to filter before the count means anything.
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

// mustAllIntents and mustAllStates exist because a loop over an empty
// enumeration asserts nothing while still reporting PASS — the exact shape
// that let a boundary walk explore 508 configurations and cross in none of
// them for two tasks. Every table below is driven by one of these
// enumerations, so an empty one would silently retire the whole table rather
// than fail it.
func mustAllIntents(t *testing.T) []Intent {
	t.Helper()
	all := AllIntents()
	if len(all) == 0 {
		t.Fatal("AllIntents() is empty; every loop it drives would assert nothing and still pass")
	}
	return all
}

func mustAllStates(t *testing.T) []State {
	t.Helper()
	all := AllStates()
	if len(all) == 0 {
		t.Fatal("AllStates() is empty; every loop it drives would assert nothing and still pass")
	}
	return all
}

// TestFinishedStatus_HasOneNonTestCaller turns TestFinishedStatus_MapsEveryOutcome's
// sole-caller sentence into something that fails when it stops being true.
//
// That sentence is load-bearing: it is the stated reason the per-Outcome table
// is driven through finishedStatus directly rather than only through
// ToSABnzbd. If a second non-test caller appeared, the direct table would stop
// covering the routing it was written to cover, and the comment would go on
// saying otherwise. A citation a reader must run by hand does not fail; this
// does.
func TestFinishedStatus_HasOneNonTestCaller(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()
	var callers []string
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		var enclosing string
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				enclosing = node.Name.Name
			case *ast.CallExpr:
				if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "finishedStatus" {
					callers = append(callers, enclosing)
				}
			}
			return true
		})
	}

	if scanned == 0 {
		t.Fatal("scanned no non-test files; this test would pass vacuously")
	}
	if want := []string{"ToSABnzbd"}; !slices.Equal(callers, want) {
		t.Errorf("non-test callers of finishedStatus = %v, want %v. "+
			"TestFinishedStatus_MapsEveryOutcome calls it directly BECAUSE ToSABnzbd is its "+
			"only non-test caller; a second one means that direct table no longer covers the "+
			"routing it was written for. Update both the claim and this list together.",
			callers, want)
	}
}
