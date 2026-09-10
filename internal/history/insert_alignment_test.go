package history

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// TestAddGetRoundTrip_EveryFieldDistinct pins AddTx's positional argument list
// against allColumns, for every column rather than a chosen sample.
//
// The failure it exists for compiles, vets and races clean. AddTx writes a
// hand-maintained column list, a run of `?` placeholders and a positional
// argument list; scanEntry reads a hand-maintained dest list. Transposing two
// same-typed neighbours in any of them — Meta and MD5Sum are both string,
// Downloaded and Completeness both int64 — is invisible to the compiler,
// because every argument is already an interface{}, and invisible to
// TestMigrations_GoldenSchema, which pins the schema and says nothing about
// the mapping between the schema and the struct.
//
// TestScanEntry_MapsColumnsByPosition covers the read half against a synthetic
// scanner. The write half was covered only incidentally, and the gap is real
// but narrower than it first looks — worth stating precisely, because the
// obvious version of this claim turned out to be false. Mutating AddTx's
// argument list and running the whole package:
//
//	Meta <-> MD5Sum                KILLED, by TestSearch_MD5Sum
//	Downloaded <-> Completeness    KILLED, by TestAddGetRoundTrip
//	Password <-> Meta              SURVIVED everything except this test
//
// So some pairs were already pinned, by tests that assert the field for an
// unrelated reason. What was missing was any test pinning ALL of them, which
// left "is this transposition caught?" depending on which fields other tests
// happened to take an interest in. This makes that coverage total rather than
// incidental.
//
// Reflection rather than a table of field names is deliberate. The table is
// the thing that goes stale — TestScanEntry_MapsColumnsByPosition's hardcoded
// indices and TestAllColumns_CountMatchesScanEntry's literal 31 both had to be
// hand-corrected when three columns were dropped, and a field added to Entry
// tomorrow would simply not be asserted here. Walking the struct means a new
// field is covered the moment it exists, and the test fails loudly rather than
// silently under-testing if a type it cannot generate a value for appears.
func TestAddGetRoundTrip_EveryFieldDistinct(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

	want := distinctEntry(t)
	if err := repo.Add(ctx, want); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := repo.Get(ctx, want.NzoID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	typ := reflect.TypeOf(want)
	gotV, wantV := reflect.ValueOf(*got), reflect.ValueOf(want)
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if name == "ID" {
			// Assigned by SQLite, not written by AddTx.
			continue
		}
		g, w := gotV.Field(i).Interface(), wantV.Field(i).Interface()
		if gt, ok := g.(time.Time); ok {
			// Stored as unix seconds; compare at that resolution.
			if gt.Unix() != w.(time.Time).Unix() {
				t.Errorf("%s: got %v, want %v — a timestamp column is transposed",
					name, gt.UTC(), w.(time.Time).UTC())
			}
			continue
		}
		if gb, ok := g.([]byte); ok {
			if string(gb) != string(w.([]byte)) {
				t.Errorf("%s: got %q, want %q", name, gb, w.([]byte))
			}
			continue
		}
		if g != w {
			t.Errorf("%s: got %v, want %v — this field's value came back in the "+
				"wrong field, so AddTx's argument order and allColumns disagree",
				name, g, w)
		}
	}
}

// distinctEntry builds an Entry whose every field holds a value unique across
// the whole struct, so that a transposition cannot be masked by two fields
// happening to share a value. Values are derived from the field NAME rather
// than its index: an index-derived value would shift wholesale if a field were
// inserted, making a genuine transposition and a benign reordering look alike
// in the failure message.
//
// It fails the test rather than skipping a field whose type it cannot fill.
// Silently leaving a new field at its zero value would make this test claim
// coverage it does not have, which is the failure mode it was written to stop.
func distinctEntry(t *testing.T) Entry {
	t.Helper()

	var e Entry
	v := reflect.ValueOf(&e).Elem()
	typ := v.Type()
	base := time.Unix(1_700_000_000, 0).UTC()

	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if name == "ID" {
			continue
		}
		f := v.Field(i)
		switch {
		case f.Type() == reflect.TypeFor[time.Time]():
			// Distinct per field, and truncated to the second because the
			// column stores unix seconds.
			f.Set(reflect.ValueOf(base.Add(time.Duration(i) * time.Hour)))
		case f.Type() == reflect.TypeFor[[]byte]():
			f.Set(reflect.ValueOf([]byte("blob-for-" + name)))
		case f.Kind() == reflect.String:
			f.SetString("value-for-" + name)
		case f.Kind() == reflect.Int64:
			// Offset so no int64 field collides with another, and non-zero so
			// a dropped argument reads as a difference rather than a default.
			f.SetInt(int64(1_000 + i))
		default:
			t.Fatalf("distinctEntry cannot generate a value for %s (%s). Add a "+
				"case for it — leaving it zero would make this test silently "+
				"stop covering that column", name, f.Type())
		}
	}
	// NzoID carries a UNIQUE constraint; the generated value is already unique
	// within this test's fresh database, but name it recognisably.
	e.NzoID = fmt.Sprintf("SABnzbd_nzo_%s", "alignment")
	return e
}
