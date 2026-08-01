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

// TestCloneJobDropsRemainingBytesCache pins the one unexported field
// cloneJob deliberately does not carry across.
//
// The exported fields are covered above, and the manifest/progress pair is
// covered by TestSnapshotJob_ArtIdxIsolation (manifest shared by reference,
// progress deep-copied). lastKnownRemainingBytes is neither: it is a cache
// that is only meaningful while progress is nil, and a snapshot clone gets
// its residency reconstructed from disk by hydrateSnapshot rather than from
// a cached figure. Copying it would hand the clone a number with no live
// state behind it, so the omission is intentional and worth pinning — a
// future reader "fixing" the apparent oversight should fail here.
func TestCloneJobDropsRemainingBytesCache(t *testing.T) {
	t.Parallel()

	job := &Job{ID: "cache-drop", Name: "cache drop"}
	job.lastKnownRemainingBytes = 4242

	if got := cloneJob(job).lastKnownRemainingBytes; got != 0 {
		t.Errorf("clone carried lastKnownRemainingBytes = %d, want 0", got)
	}
}

// TestCloneJobCarriesManifestScalars pins the opposite property from
// TestCloneJobDropsRemainingBytesCache: the five manifest-derived scalars
// (totalBytes, numFiles, numArticles, par2Bytes, par2Files) are unexported,
// so TestCloneJobCopiesEveryExportedField's reflective sweep does not see
// them. They must still be copied — unlike lastKnownRemainingBytes, they are
// immutable and residency-independent, and a reporting path relies on them
// surviving a snapshot without hydration.
func TestCloneJobCarriesManifestScalars(t *testing.T) {
	t.Parallel()

	job := &Job{ID: "scalars-carry", Name: "scalars carry"}
	job.totalBytes = 111
	job.numFiles = 2
	job.numArticles = 3
	job.par2Bytes = 44
	job.par2Files = 1

	cp := cloneJob(job)
	if got := cp.TotalBytes(); got != 111 {
		t.Errorf("cloneJob dropped totalBytes: got %d, want 111", got)
	}
	if got := cp.NumFiles(); got != 2 {
		t.Errorf("cloneJob dropped numFiles: got %d, want 2", got)
	}
	if got := cp.NumArticles(); got != 3 {
		t.Errorf("cloneJob dropped numArticles: got %d, want 3", got)
	}
	if got := cp.Par2Bytes(); got != 44 {
		t.Errorf("cloneJob dropped par2Bytes: got %d, want 44", got)
	}
	if got := cp.Par2Files(); got != 1 {
		t.Errorf("cloneJob dropped par2Files: got %d, want 1", got)
	}
}
