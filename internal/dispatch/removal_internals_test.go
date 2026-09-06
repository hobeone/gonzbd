package dispatch

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestAdmitsLocked_TruthTable pins the single predicate every guard now
// consults. It is worth a direct test rather than only its callers' tests
// because it is the one place the invariant is written down: before #513 the
// same expression had five copies, and a sixth reader disagreeing with the
// others would have been invisible at all of them.
func TestAdmitsLocked_TruthTable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		registered bool
		removing   int
		want       bool
	}{
		{"registered and idle", true, 0, true},
		{"registered, one removal outstanding", true, 1, false},
		{"registered, two removals outstanding", true, 2, false},
		{"not registered", false, 0, false},
		{"not registered, stale marker", false, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDispatcher(t)
			if tc.registered {
				if err := d.Add(job.New("j1", "n", job.Policy{}), Header{}); err != nil {
					t.Fatalf("Add: %v", err)
				}
			}
			d.mu.Lock()
			if tc.removing > 0 {
				d.removing["j1"] = tc.removing
			}
			got := d.admitsLocked("j1")
			d.mu.Unlock()

			if got != tc.want {
				t.Errorf("admitsLocked(j1) = %v, want %v (registered=%v removing=%d)",
					got, tc.want, tc.registered, tc.removing)
			}
		})
	}
}

// TestBeginRemovalIfIdle_Outcomes pins its three answers, and in particular
// that a refusal sets NO marker: a caller told the job is live walks away, and
// a marker left behind by that refusal would make the job permanently
// unadmittable with nothing left holding a token to release it.
func TestBeginRemovalIfIdle_Outcomes(t *testing.T) {
	t.Run("not registered", func(t *testing.T) {
		d := newTestDispatcher(t)
		rm, live := d.beginRemovalIfIdle("nope")
		if rm != nil || live {
			t.Fatalf("beginRemovalIfIdle on an unregistered job = (%v, %v), want (nil, false)", rm, live)
		}
	})

	t.Run("idle mints a token and marks", func(t *testing.T) {
		d := newTestDispatcher(t)
		if err := d.Add(job.New("j1", "n", job.Policy{}), Header{}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		rm, live := d.beginRemovalIfIdle("j1")
		if rm == nil || live {
			t.Fatalf("beginRemovalIfIdle on an idle job = (%v, %v), want (token, false)", rm, live)
		}
		d.mu.Lock()
		admits := d.admitsLocked("j1")
		d.mu.Unlock()
		if admits {
			t.Error("job still admits work while a removal token is outstanding")
		}
	})

	t.Run("launched refuses and leaves no marker", func(t *testing.T) {
		d := newTestDispatcher(t)
		if err := d.Add(job.New("j1", "n", job.Policy{}), Header{}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if !d.claimLaunched("j1") {
			t.Fatal("setup: claimLaunched(j1) = false on a fresh dispatcher")
		}

		rm, live := d.beginRemovalIfIdle("j1")
		if rm != nil || !live {
			t.Fatalf("beginRemovalIfIdle on a launched job = (%v, %v), want (nil, true)", rm, live)
		}
		d.mu.Lock()
		marker := d.removing["j1"]
		d.mu.Unlock()
		if marker != 0 {
			t.Errorf("d.removing[j1] = %d after a refusal, want 0 — a refused caller holds no "+
				"token, so a marker set here would never be released", marker)
		}
	})
}

// TestDeregister_IsTotal pins the property the Dispatcher struct's own comment
// calls out as having been got wrong three times on this branch: "ADDING A MAP
// HERE MEANS EXTENDING EVERY TEARDOWN."
//
// The failure mode is silent — a stale entry only bites a job ID that comes
// back, and a reused ID reads as already resident (so it never hydrates) and
// already launched (so it is never launchable). Nothing errors; the job simply
// never runs.
//
// This is the enumeration-as-test that Standing Design Rule 4 prescribes for a
// population a machine can check. The maps are populated directly rather than
// through their accessors because the subject is deregister's coverage of
// them, not how they come to be occupied — and a new map added to the struct
// but not to deregister leaves this test failing on a name a reviewer can act
// on.
func TestDeregister_IsTotal(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.Add(job.New("j1", "n", job.Policy{}), Header{Name: "n"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.mu.Lock()
	d.written["j1"] = Persisted{ID: "j1"}
	d.resident["j1"] = true
	d.removing["j1"] = 1
	d.occupiers["j1"] = 1
	d.occupancyTokens["j1"] = map[any]struct{}{new(byte): {}}
	d.occupyDrained["j1"] = make(chan struct{})
	d.occupyStep["j1"] = make(chan struct{})
	d.launched["j1"] = make(chan struct{})
	d.mu.Unlock()

	d.deregister("j1")

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, m := range []struct {
		name    string
		present bool
	}{
		{"byID", d.byID["j1"] != nil},
		{"written", func() bool { _, ok := d.written["j1"]; return ok }()},
		{"resident", func() bool { _, ok := d.resident["j1"]; return ok }()},
		{"removing", func() bool { _, ok := d.removing["j1"]; return ok }()},
		{"occupiers", func() bool { _, ok := d.occupiers["j1"]; return ok }()},
		{"occupancyTokens", func() bool { _, ok := d.occupancyTokens["j1"]; return ok }()},
		{"occupyDrained", func() bool { _, ok := d.occupyDrained["j1"]; return ok }()},
		{"occupyStep", func() bool { _, ok := d.occupyStep["j1"]; return ok }()},
		{"launched", func() bool { _, ok := d.launched["j1"]; return ok }()},
	} {
		if m.present {
			t.Errorf("d.%s still holds j1 after deregister — a teardown was not extended when "+
				"that map was added, and the symptom is a reused ID that silently never runs", m.name)
		}
	}
	for _, id := range d.order {
		if id == "j1" {
			t.Error("d.order still holds j1 after deregister")
		}
	}
}

// TestBeginRemovalLocked_IsTheSoleConstructor pins that the marker and the
// token are raised together. They were minted at two sites before review
// caught it, which is the two-constructors smell Rule 2 names — the same shape
// that let newManifest and UnmarshalJSON diverge over totalBytes.
func TestBeginRemovalLocked_IsTheSoleConstructor(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.Add(job.New("j1", "n", job.Policy{}), Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.mu.Lock()
	rm := d.beginRemovalLocked("j1")
	marker := d.removing["j1"]
	d.mu.Unlock()

	if rm == nil {
		t.Fatal("beginRemovalLocked returned no token")
	}
	if marker != 1 {
		t.Errorf("d.removing[j1] = %d, want 1 — the token and the marker must be raised together, "+
			"or a holder can release a marker that was never set", marker)
	}
	if rm.id != "j1" || rm.d != d {
		t.Errorf("token = {id:%q d:%p}, want {id:\"j1\" d:%p}", rm.id, rm.d, d)
	}

	rm.abort()
	d.mu.Lock()
	_, still := d.removing["j1"]
	d.mu.Unlock()
	if still {
		t.Error("marker survived abort of the token that raised it")
	}
}
