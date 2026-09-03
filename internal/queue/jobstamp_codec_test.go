package queue

import (
	"testing"
	"time"
)

// TestJobStampCodec_RoundTripsAndAgreesWithTheOwnersBound pins the two halves
// of #464 that have to agree: what the owner will hold in memory, and what the
// store's integer column can represent.
//
// 0 is the column's "absent" sentinel, so the codec is only unambiguous if no
// stamp the owner ACCEPTS encodes to 0. That equivalence is what let #464 close
// without making the columns nullable, so it is asserted rather than argued for
// in a comment.
//
// It stayed in internal/queue when progress_test.go moved to internal/job:
// encodeJobStamp/decodeJobStamp are the SQLite column's wire form and live in
// sqlite_store.go, and this test's whole subject is their agreement with the
// in-memory owner's bound.
//
// The load-bearing assertion is the PRE-EPOCH one at the end, not the
// sign-agreement loop before it — see the comment there for why the loop is
// circular and what it is still worth.
func TestJobStampCodec_RoundTripsAndAgreesWithTheOwnersBound(t *testing.T) {
	t.Parallel()

	real := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	if got := encodeJobStamp(real); got != real.Unix() {
		t.Errorf("encodeJobStamp(%v) = %d, want %d", real, got, real.Unix())
	}
	if got := encodeJobStamp(time.Time{}); got != 0 {
		t.Errorf("encodeJobStamp(zero) = %d, want 0", got)
	}
	if got := decodeJobStamp(real.Unix()); !got.Equal(real) {
		t.Errorf("decodeJobStamp(%d) = %v, want %v", real.Unix(), got, real)
	}
	for _, absent := range []int64{0, -1} {
		if got := decodeJobStamp(absent); !got.IsZero() {
			t.Errorf("decodeJobStamp(%d) = %v, want the zero value", absent, got)
		}
	}

	for _, tc := range []time.Time{
		{}, time.Unix(0, 0), time.Unix(0, 500000000), time.Unix(1, 0), real,
	} {
		if isJobStamp(tc) != (encodeJobStamp(tc) > 0) {
			t.Errorf("isJobStamp(%v) = %v but encodeJobStamp gives %d",
				tc, isJobStamp(tc), encodeJobStamp(tc))
		}
	}

	// The loop above compares signs, and encodeJobStamp is implemented in
	// terms of isJobStamp, so it holds by construction today — it is a change
	// detector for a future reimplementation, not a proof.
	//
	// This is the assertion that is not circular. A plausible rewrite guarding
	// on IsZero rather than isJobStamp passes every assertion above, because
	// every value they cover has Unix() == 0 either way; it differs only for a
	// pre-epoch time, which it would write to the column verbatim. The column
	// must hold a positive stamp or the absent sentinel and nothing else, so
	// pin the value rather than its sign.
	for _, preEpoch := range []time.Time{time.Unix(-1, 0), time.Date(1969, 7, 20, 20, 17, 0, 0, time.UTC)} {
		if got := encodeJobStamp(preEpoch); got != 0 {
			t.Errorf("encodeJobStamp(%v) = %d, want the 0 sentinel — a negative "+
				"reaches the column as a value the decode reads as absent anyway, "+
				"so the two disagree about what was stored", preEpoch, got)
		}
	}
}
