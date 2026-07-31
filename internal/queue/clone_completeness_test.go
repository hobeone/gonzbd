package queue

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// TestCloneJobCopiesEveryExportedField guards the hazard introduced when
// cloneJob stopped being a whole-struct copy.
//
// cloneJob used to be `cp := *j`, which picked up new Job fields for free.
// Job now holds a sync.RWMutex value (residencyMu), so a whole-struct copy
// is a go vet copylocks violation and cloneJob enumerates the exported
// fields by hand instead. That enumeration cannot notice a field added to
// Job later — the clone would silently drop it, and every snapshot consumer
// would see a zero value with no compile error and no test failure anywhere
// near the change.
//
// This test closes that gap from the other side: it fills every exported
// field via reflection and asserts the clone round-trips it. Adding a field
// to Job without adding it to cloneJob fails here. Adding a field whose type
// the filler below does not know how to populate also fails here, rather
// than passing vacuously against a zero value.
func TestCloneJobCopiesEveryExportedField(t *testing.T) {
	t.Parallel()

	var job Job
	v := reflect.ValueOf(&job).Elem()
	typ := v.Type()

	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		if !fillDistinctive(v.Field(i), i) {
			t.Fatalf("field %s has type %s, which this test does not know how to "+
				"populate; extend fillDistinctive so the field is really checked "+
				"and confirm cloneJob copies it", f.Name, f.Type)
		}
	}

	cp := cloneJob(&job)

	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		want := v.Field(i).Interface()
		got := reflect.ValueOf(cp).Elem().Field(i).Interface()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("cloneJob dropped or altered field %s: got %#v, want %#v",
				f.Name, got, want)
		}
	}
}

// fillDistinctive sets fv to a non-zero value and reports whether it knew
// how to. idx is the field's index within the struct, used to make the
// value unique per field rather than merely non-zero: a clone that copied
// the wrong same-typed field into this one — Filename into Name, say, or
// Added into AvgAge — writes a value that is still non-zero and would
// survive a plain zero-check. Only bool cannot be made unique this way, and
// a bool field mixed up with another bool is the one substitution this test
// cannot see.
func fillDistinctive(fv reflect.Value, idx int) bool {
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(fmt.Sprintf("distinctive-field-%d", idx))
		return true
	case reflect.Bool:
		fv.SetBool(true)
		return true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		fv.SetInt(int64(idx) + 1)
		return true
	case reflect.Map:
		if fv.Type() != reflect.TypeFor[map[string][]string]() {
			return false
		}
		fv.Set(reflect.ValueOf(map[string][]string{
			fmt.Sprintf("k%d", idx): {"v1", "v2"},
		}))
		return true
	case reflect.Slice:
		if fv.Type() != reflect.TypeFor[[]string]() {
			return false
		}
		fv.Set(reflect.ValueOf([]string{fmt.Sprintf("alt.binaries.test%d", idx)}))
		return true
	case reflect.Struct:
		if fv.Type() != reflect.TypeFor[time.Time]() {
			return false
		}
		fv.Set(reflect.ValueOf(
			time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC).Add(time.Duration(idx) * time.Hour),
		))
		return true
	default:
		return false
	}
}
