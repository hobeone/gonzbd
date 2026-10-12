package dispatch

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// unsafeJobNames are names that are not one safe path component. Each becomes
// DownloadDir/<name>, and filepath.Join cleans "..", so every one of them
// would put a job's files somewhere other than its own directory under
// DownloadDir.
var unsafeJobNames = []string{"", ".", "..", "a/../../etc", "a/b", `a\b`, "/abs", "x\x00y"}

// TestAdd_RefusesANameThatIsNotOnePathComponent pins that registration applies
// the same name check as SetName: a name that is not one safe path component
// is refused with ErrInvalidJobName, leaves no entry and writes no row.
func TestAdd_RefusesANameThatIsNotOnePathComponent(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	for _, name := range unsafeJobNames {
		err := d.Add(context.Background(), job.New("a", name, job.PolicyFromPP(3)), Header{Name: name})
		if !errors.Is(err, ErrInvalidJobName) || errors.Is(err, ErrJobNameTaken) {
			t.Errorf("Add under %q = %v, want ErrInvalidJobName and not ErrJobNameTaken", name, err)
		}
		if _, ok := d.Row("a"); ok {
			t.Fatalf("a refused Add under %q left the job registered", name)
		}
	}
	st.mu.Lock()
	_, wrote := st.rows["a"]
	st.mu.Unlock()
	if wrote {
		t.Fatal("a refused Add wrote a queue row")
	}
	// The job's own name is what its directory is joined from, so a header
	// that names a safe directory cannot carry a job whose name is unsafe.
	err := d.Add(context.Background(), job.New("a", "a/../../etc", job.PolicyFromPP(3)), Header{Name: "safe"})
	if !errors.Is(err, ErrInvalidJobName) {
		t.Errorf("Add of a job named %q under the header name %q = %v, want ErrInvalidJobName", "a/../../etc", "safe", err)
	}
	if err := addNamed(t, d, "a", "a .. .. etc"); err != nil {
		t.Errorf("Add under a name with dots and spaces but no separator = %v, want nil", err)
	}
}

// TestReserveName_RefusesANameThatIsNotOnePathComponent pins that a retry
// cannot reserve, and so cannot restore a directory to, a name that is not
// one safe path component.
func TestReserveName_RefusesANameThatIsNotOnePathComponent(t *testing.T) {
	d := newTestDispatcher(t)
	for _, name := range unsafeJobNames {
		release, err := d.ReserveName("a", name)
		if !errors.Is(err, ErrInvalidJobName) || errors.Is(err, ErrJobNameTaken) {
			t.Errorf("ReserveName(a, %q) = %v, want ErrInvalidJobName and not ErrJobNameTaken", name, err)
		}
		if release != nil {
			release()
		}
	}
}

// TestRestore_SkipsARowWhoseNameIsNotOnePathComponent pins that restore, the
// registry's other entrance, applies the check too: the stored row is the one
// place a name arrives from outside this process's own admission.
func TestRestore_SkipsARowWhoseNameIsNotOnePathComponent(t *testing.T) {
	var buf bytes.Buffer
	st := &fakeStore{}
	st.seed([]Persisted{
		{ID: "bad", SortKey: 1, Header: Header{Name: "a/../../etc"}, State: job.StateView{State: job.Fetching}},
		{ID: "good", SortKey: 2, Header: Header{Name: "good"}, State: job.StateView{State: job.Fetching}},
	})
	d := newTestDispatcher(t, withStore(st), unrestored())
	d.log = captureLogger(&buf)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	got := d.List()
	if len(got) != 1 || got[0].ID != "good" {
		t.Fatalf("List() = %+v, want [good]", got)
	}
	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "job_id=bad") {
		t.Errorf("log output %q does not report the refused row", out)
	}
}
