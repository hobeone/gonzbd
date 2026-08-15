package assembler

import (
	"slices"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestDrainAndClose_RoutesTheRolledBackArticlesAndTheFault pins the one
// FileWriter-failure path that used to route neither.
//
// drainAndClose logged its Drain, Sync and Close errors at Warn and discarded
// all three. That is the same shape as the accept and pressure-flush paths
// this package already fixed, arriving at the fourth place a write can fail,
// and it is the one place where NOTHING else picks the failure up:
//
//   - The articles. A failed Drain rolls back everything after the write that
//     failed into w.faulted, and takeFaulted is its only consumer. Without a
//     releaseFaulted here that set dies with the writer, so the articles keep
//     their Emitted bits, ForEachUnfinishedArticle skips them, and only a
//     restart's ClearAllEmitted recovers them. Not Done, not Failed, not
//     Outstanding.
//   - The fault. handleSyncOp's opClose arm leaves r.err nil, so the barrier
//     is told the close SUCCEEDED. Unlike the opDrain arm — where the barrier
//     routes the fault and this package must not route it twice — there is no
//     second reporter here at all.
//
// The CloseJobHandles caller is the one that bites: it also marks the file
// completed, so a later write for those articles is dropped as a late
// duplicate. The job then waits on articles nothing will re-dispatch.
func TestDrainAndClose_RoutesTheRolledBackArticlesAndTheFault(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	var rolledBack []int32
	a.opts.OnArticlesUnwritten = func(_ string, _ int, artIdxs []int32) {
		rolledBack = append(rolledBack, artIdxs...)
	}
	var faults []*storagefault.Fault
	a.opts.OnWriteFault = func(_ string, _ int, f *storagefault.Fault) { faults = append(faults, f) }

	f := newHelperFile(t, dir, "job1_0.dat", 0)
	f.w.wc = newWriteCache(1 << 20)
	f.info.TotalParts = 3

	for i := range 2 {
		if !a.handleSuccessArticle(f, WriteRequest{
			JobID: "job", FileIdx: 0, ArtIdx: int32(i), //nolint:gosec // G115: tiny test index
			MessageID: string(rune('a' + i)), Offset: int64(i) * 4, Data: []byte("AAAA"),
		}) {
			t.Fatalf("article %d was not accepted, so the fixture never buffered it", i)
		}
		f.partsWritten++
	}

	// The device fills up between the accepts and the close.
	f.w.writeAt = func([]byte, int64) (int, error) { return 0, syscall.ENOSPC }

	a.drainAndClose(f)

	slices.Sort(rolledBack)
	if !slices.Equal(rolledBack, []int32{0, 1}) {
		t.Errorf("rolled-back articles = %v, want [0 1] — nothing else reports these: "+
			"the opClose arm returns a nil error, so the barrier is told the close "+
			"succeeded, and an article reported by nobody keeps its Emitted bit and "+
			"is never re-dispatched", rolledBack)
	}
	if f.partsWritten != 0 {
		t.Errorf("partsWritten = %d, want 0 — the file is left that many parts closer "+
			"to TotalParts with nothing on disk behind them", f.partsWritten)
	}
	if len(faults) != 1 {
		t.Fatalf("routed %d faults, want 1 — a full device at close reached neither "+
			"Stallable nor Fail, so the job was not parked and no reason was surfaced (A2)",
			len(faults))
	}
	if !faults[0].Permanent && faults[0].Op != "write" {
		t.Errorf("fault = %+v, want the write op preserved", faults[0])
	}
}

// TestDrainAndClose_RoutesAFailingSyncToo covers the second of the three
// operations. A Drain that writes everything successfully and an fsync that
// then fails means the bytes are NOT durable, which is exactly the condition
// R19 asks to be surfaced — and it rolls back no articles, so releaseFaulted
// alone would leave it silent.
func TestDrainAndClose_RoutesAFailingSyncToo(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	var faults []*storagefault.Fault
	a.opts.OnWriteFault = func(_ string, _ int, f *storagefault.Fault) { faults = append(faults, f) }

	f := newHelperFile(t, dir, "job2_0.dat", 0)
	f.w.syncFile = func() error { return syscall.EIO }

	a.drainAndClose(f)

	if len(faults) != 1 {
		t.Fatalf("routed %d faults, want 1 — an fsync that fails at close means the "+
			"drained bytes are not durable, and nothing else reports it", len(faults))
	}
	if faults[0].Op != "sync" {
		t.Errorf("fault op = %q, want \"sync\" — relabelling it discards which syscall "+
			"actually failed, which is the whole of what makes the reason actionable (R27)",
			faults[0].Op)
	}
}
