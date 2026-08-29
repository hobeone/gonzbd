package dispatch

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

func TestAdd_RejectsADuplicateID(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{Name: "n"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := d.Add(job.New("j1", "other", job.Policy{}), Header{Name: "other"}); err == nil {
		t.Fatal("second Add with the same ID returned nil, want an error — the registry is keyed by ID and a silent overwrite would strand the first job's resources")
	}
}

func TestList_PreservesInsertionOrder(t *testing.T) {
	d := newTestDispatcher(t)
	for _, id := range []string{"c", "a", "b"} {
		if err := d.Add(job.New(id, id, job.Policy{}), Header{Name: id}); err != nil {
			t.Fatalf("Add(%s): %v", id, err)
		}
	}
	got := d.List()
	want := []string{"c", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("List returned %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("row %d is %s, want %s — queue order is the priority policy and List must not reorder it", i, got[i].ID, want[i])
		}
	}
}

func TestList_CarriesTheHeaderAndTheView(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	h := Header{Name: "movie", Category: "tv", Priority: 2, Bytes: 4096}
	if err := d.Add(j, h); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got := d.List()
	if len(got) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(got))
	}
	if got[0].Header != h {
		t.Errorf("Header = %+v, want %+v", got[0].Header, h)
	}
	if got[0].View != d.q.Render(j) {
		t.Errorf("View = %+v, want %+v", got[0].View, d.q.Render(j))
	}
}

func TestList_EmptyRegistryReturnsEmptyNonNil(t *testing.T) {
	d := newTestDispatcher(t)
	got := d.List()
	if got == nil {
		t.Fatal("List() = nil on an empty registry, want an empty non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("List() has %d rows, want 0", len(got))
	}
}
