package job

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/hobeone/gonzbd/internal/durability"
)

// JobProgress is the mutable per-article and per-file state of a job:
// which articles are done/failed/emitted, per-file assembly bookkeeping,
// and job-level counters. Always sized to match its Manifest's
// NumArticles()/NumFiles(). Deep-copied on every Snapshot/SnapshotJob
// (unlike Manifest, which is shared).
//
//nolint:revive // stutter is preserved for consistency during queue -> job transition
type JobProgress struct {
	// Flat, global article index. Bitsets rather than []bool: see bitset.go
	// for the memory argument. JobProgress is not serialized, so none of the
	// three is persisted, and a restart builds each job's progress afresh
	// (newJobProgressSized). emitted in particular must not survive a
	// restart: an article the assembler had not yet written must be
	// re-dispatched.
	//
	// The done bit is what marks an article as resolved, and nothing sets it
	// from dispatch. Its doors are MarkArticleWritten, which the recorder
	// calls once the article's WriteAt has returned (internal/app/record.go);
	// installRows, through InstallVerified and InstallFileVerification, which
	// install rows a restart or retry read back and fsynced, or rows of a
	// complete=1 file whose fsync preceded the flag; and setFailedBits, for
	// an article whose bytes will never arrive (that failure is in memory
	// only: it is not persisted). Of the functions that reach markDone,
	// MarkArticleWritten and installRows are the only two:
	// `git grep -n -E '\bmarkDone\(' -- '*.go' ':!*_test.go'` finds 3 lines,
	// the definition and one call in each.
	//
	// So a Done bit from MarkArticleWritten stands on a write, not on an
	// fsync. What stops it outliving bytes that never reached the disk is the
	// untrust: a failed fsync at completion (FileWriter.finish), at
	// CloseJobHandles or at worker exit untrusts the file
	// (Application.handleFileUntrusted), which clears its Done bits and
	// removes its rows from the record (docs/durability-contract.md §4). What
	// a crash loses before any fsync is caught by the next start, which reads
	// back every row of a complete=0 file before installing it; a complete=1
	// file is installed as it stands, because complete=1 is written only after
	// the file's fsync.
	//
	// TestDoneBitWriters_MatchTheEnumerationStatedInProse enforces the list
	// of doors above. Add a door onto the bit and it fails by name.
	done, failed, emitted bitset
	files                 []FileProgress

	// written holds, per file index, the written rows of a file that has not
	// yet completed, for the whole-file CRC: verified rows and rows written
	// this process, in no particular order. It is not persisted here:
	// written_articles is the record, and this is a resident copy of the part
	// of it a completion still needs. verified.go owns its lifecycle.
	written map[int][]durability.WrittenRow

	pendingArticles   int
	articlesResolved  int
	articlesFailed    int
	earlyAborted      bool
	failedBytes       int64
	serverStats       map[string]int64
	downloadStarted   time.Time
	downloadFinished  time.Time
	par2Recovered     bool
	par2ReleaseReason string
}

// FetchPolicy records whether the job intends to download a file. It
// replaces a Deferred bool so that "held pending a verdict" and "proven
// unnecessary" cannot both be true, and so that every read site has to say
// which of the two it means.
type FetchPolicy uint8

const (
	// FetchAlways is every content file, the par2 index, and any recovery
	// volume the job has decided to fetch after all. It is the zero value
	// because it is the ordinary case for every file in a job.
	FetchAlways FetchPolicy = iota
	// FetchIfNeeded is a par2 recovery volume held back until the CRC
	// oracle rules on whether repair is needed.
	FetchIfNeeded
	// FetchNever is a recovery volume the oracle proved unnecessary. Its
	// manifest entry and job_files row stay; only the intent changes.
	FetchNever
)

// AllFetchPolicies lists every declared policy so a test can walk them and
// assert that a switch over them handles each one rather than falling through
// silently. Kept in declaration order.
//
// This enum is read through two predicates that mean different things —
// `!= FetchAlways` for dispatch, completion and byte accounting, and
// `== FetchIfNeeded` for HasDeferredPar2 and DeferredRecoveryIndices, which
// gate CRC re-verification and whether a late failure may re-arm a volume. A
// fourth value would need a decision at each of those sites, and the failure
// mode of missing one is silence: a policy matching neither predicate is
// excluded from every aggregate and invisible to the un-defer path, so its
// file is never fetched and never blocks completion.
//
// It is hand-written, which on its own would make it a second copy of the
// enum carrying the same defect: a value added to the const block but not
// here is invisible to every loop over it, and every exhaustiveness test
// built on it passes vacuously. TestAllFetchPolicies_Exhaustive closes that
// loop by parsing the const block itself, the same way
// postproc.AllQuickCheckOutcomes (#313) and constants.AllStatuses (#291) are
// pinned.
func AllFetchPolicies() []FetchPolicy {
	return []FetchPolicy{
		FetchAlways,
		FetchIfNeeded,
		FetchNever,
	}
}

// FileProgress is one file's mutable assembly state.
type FileProgress struct {
	Complete bool
	// Fetch records whether this file will be downloaded. See FetchPolicy.
	Fetch   FetchPolicy
	Pending int
	// Bytes is the file's NZB-claimed size, copied from the manifest when the
	// file is first seen so RemainingBytes derives from progress alone and
	// keeps working after the manifest is evicted.
	//
	Bytes int64
	// BytesDownloaded is the sum of Bytes over this file's resolved,
	// non-failed articles. It is in the SAME unit as Bytes above — the
	// encoded NZB `bytes` attribute — because RemainingBytes subtracts the
	// two, and only figures in one unit can be subtracted.
	//
	// That is not the unit the written_articles rows work in. A row's Length is
	// the DECODED payload handed to WriteAt, summed over the same set of
	// articles, and a few percent lower. The two are not interchangeable, which
	// is why this is summed from the manifest's per-article bytes over the
	// resolved set rather than read off the record.
	//
	// Maintained live by markDone and rebuilt by recompute at hydration, both
	// from the manifest.
	BytesDownloaded int64
	// FailedBytes is the sum of bytes belonging to this file's permanently
	// failed articles. Carried per file, not just job-wide, so remaining
	// derives from progress alone: a failed article was never downloaded, so
	// BytesDownloaded does not account for it, and without this the derivation
	// would report its bytes as still to fetch forever.
	//
	// Not persisted, and does not need to be: markFailed below computes it as
	// m.ArticleBytes(i) summed over the failed set. The manifest knows an
	// article's size whether or not it was ever fetched. The failed set itself
	// is in memory only: across a restart a complete=1 file rebuilds it as the
	// complement of its written rows (InstallFileVerification), and a complete=0
	// file's failed articles are Outstanding again.
	FailedBytes int64
	// IsPar2 marks a par2 file — the index or a recovery volume — as opposed
	// to content. Carried per file, like Bytes and FailedBytes, so
	// ContentFailedBytes derives from progress alone at any residency.
	//
	// Note this is broader than the manifest's own per-file par2-recovery
	// flag, which marks only volumes. The index matters here precisely because
	// it is not a recovery volume: it is fetched, so it can fail, and its
	// failure is not damage. Classification is by subject, done once from the
	// manifest when the file is first seen.
	IsPar2 bool
	// There is deliberately no WriteCursor or MaxWritten here.
	//
	// Both used to be persisted and fed back to the assembler on resume, so
	// the completion truncate would not cut below what earlier runs wrote
	// (#342). The truncate no longer derives its bound from anything the
	// queue knows: the assembler's FileWriter.finish trims to the end of the
	// last range it owns, and a restart's verifier to the end of the last
	// verified row, so it needs no seed. The write cursor was only ever a
	// coalescing hint, and went with the assembler's write cache (#311, #353).
	//
	// They are gone rather than retained-at-zero because a field that is
	// always zero and documented as a resume seed is worse than no field: a
	// reader chasing #342 would find it, read that the truncate depends on
	// it, see it return 0, and conclude the bug is back.
	Filename       string // resolved on-disk filename; empty until resolved
	AssembledCRC32 uint32
}

const (
	earlyAbortSample    = 10
	earlyAbortThreshold = 0.80
)

// FileMeta is the per-file sizing a fresh JobProgress is built from. It
// carries no state: every article starts Outstanding and every file
// incomplete, at the FetchAlways zero.
type FileMeta struct {
	ArticleCount int
	Bytes        int64
	IsPar2       bool
}

// newJobProgressSized returns a fresh JobProgress sized to files: every
// file's Pending is its article count, and no bit is set.
func newJobProgressSized(files []FileMeta) *JobProgress {
	total := 0
	for _, f := range files {
		total += f.ArticleCount
	}
	p := &JobProgress{
		done:            newBitset(total),
		failed:          newBitset(total),
		emitted:         newBitset(total),
		files:           make([]FileProgress, len(files)),
		pendingArticles: total,
	}
	for fi, f := range files {
		p.files[fi].Pending = f.ArticleCount
		p.files[fi].IsPar2 = f.IsPar2
		p.files[fi].Bytes = f.Bytes
	}
	return p
}

// fileMetaFromManifest projects m into the per-file sizing
// newJobProgressSized takes.
//
// The projection is lossless for what JobProgress needs: Manifest.TotalBytes
// is the sum of every file's bytes, and Manifest.NumArticles is the sum of
// every file's article count, so the totals newJobProgressSized derives are
// the manifest's.
func fileMetaFromManifest(m *Manifest) []FileMeta {
	files := make([]FileMeta, m.NumFiles())
	for fi := range files {
		lo, hi := m.FileRange(fi)
		files[fi] = FileMeta{
			ArticleCount: hi - lo,
			Bytes:        m.FileBytes(fi),
			IsPar2:       isPar2File(m.FileSubject(fi)),
		}
	}
	return files
}

// newJobProgress returns a fresh JobProgress sized to m: every file's Pending
// starts at its article count and pendingArticles at m.NumArticles(), so
// RemainingBytes() — derived from per-file state, see derivedRemainingBytes —
// starts at m.TotalBytes().
func newJobProgress(m *Manifest) *JobProgress {
	return newJobProgressSized(fileMetaFromManifest(m))
}

// NumFiles returns the number of files tracked by the progress record.
func (p *JobProgress) NumFiles() int {
	if p == nil {
		return 0
	}
	return len(p.files)
}

// TotalArticles returns the total number of articles tracked by the progress record.
func (p *JobProgress) TotalArticles() int {
	if p == nil {
		return 0
	}
	return p.done.Len()
}

// ArticleDone reports whether global article index i has resolved (success or failure).
func (p *JobProgress) ArticleDone(i int) bool {
	if p == nil || i < 0 || i >= p.done.Len() {
		return false
	}
	return p.done.Get(i)
}

// ArticleFailed reports whether global article index i permanently failed.
func (p *JobProgress) ArticleFailed(i int) bool {
	if p == nil || i < 0 || i >= p.failed.Len() {
		return false
	}
	return p.failed.Get(i)
}

// ArticleEmitted reports whether global article index i has an in-flight result
// handed to the assembler but not yet made durable.
func (p *JobProgress) ArticleEmitted(i int) bool {
	if p == nil || i < 0 || i >= p.emitted.Len() {
		return false
	}
	return p.emitted.Get(i)
}

// FileComplete reports whether file fileIdx has been fully assembled on disk.
func (p *JobProgress) FileComplete(fi int) bool {
	if p == nil || fi < 0 || fi >= len(p.files) {
		return false
	}
	return p.files[fi].Complete
}

// FileFetchPolicy reports whether file fi will be downloaded, and if not,
// why. Out-of-range and nil receivers report FetchAlways, matching the
// permissive convention of the accessors either side of it.
func (p *JobProgress) FileFetchPolicy(fi int) FetchPolicy {
	if p == nil || fi < 0 || fi >= len(p.files) {
		return FetchAlways
	}
	return p.files[fi].Fetch
}

// FilePending returns the count of not-yet-resolved articles in file fileIdx.
func (p *JobProgress) FilePending(fi int) int {
	if p == nil || fi < 0 || fi >= len(p.files) {
		return 0
	}
	return p.files[fi].Pending
}

// FileBytesDownloaded returns the sum of successfully downloaded article bytes in file fileIdx.
func (p *JobProgress) FileBytesDownloaded(fi int) int64 {
	if p == nil || fi < 0 || fi >= len(p.files) {
		return 0
	}
	return p.files[fi].BytesDownloaded
}

// FileFailedBytes returns the sum of bytes belonging to permanently failed
// articles in file fileIdx.
func (p *JobProgress) FileFailedBytes(fi int) int64 {
	if p == nil || fi < 0 || fi >= len(p.files) {
		return 0
	}
	return p.files[fi].FailedBytes
}

// FileFilename returns the resolved on-disk filename for file fileIdx, or empty if unresolved.
func (p *JobProgress) FileFilename(fi int) string {
	if p == nil || fi < 0 || fi >= len(p.files) {
		return ""
	}
	return p.files[fi].Filename
}

// FileAssembledCRC32 returns the assembled CRC32 for file fileIdx, or zero if unavailable.
func (p *JobProgress) FileAssembledCRC32(fi int) uint32 {
	if p == nil || fi < 0 || fi >= len(p.files) {
		return 0
	}
	return p.files[fi].AssembledCRC32
}

// PendingArticles returns the count of not-yet-resolved articles across all files.
func (p *JobProgress) PendingArticles() int {
	if p == nil {
		return 0
	}
	return p.pendingArticles
}

// ArticlesResolved returns the count of articles that have resolved (success or failure).
func (p *JobProgress) ArticlesResolved() int {
	if p == nil {
		return 0
	}
	return p.articlesResolved
}

// ArticlesFailed returns the count of articles that have permanently failed.
func (p *JobProgress) ArticlesFailed() int {
	if p == nil {
		return 0
	}
	return p.articlesFailed
}

// EarlyAborted reports whether the early-abort heuristic has already fired for this job.
func (p *JobProgress) EarlyAborted() bool {
	if p == nil {
		return false
	}
	return p.earlyAborted
}

// ContentFailedBytes returns the failed bytes that represent damaged content —
// FailedBytes minus everything lost from par2 files.
//
// This is the figure a repair decision needs, and it is not the same question
// FailedBytes answers. Repair capacity rebuilds damaged *content*; a par2 file
// is not content. When an index or a recovery volume fails to download, no
// content became unrecoverable — the job merely has less capacity than its
// manifest advertised, which the capacity side of the comparison already
// reflects. Counting those bytes as damage condemns a job for losing a file
// whose only purpose was to rescue other files.
//
// The failure mode this prevents is not hypothetical. For a par2 set with an
// index and no recovery volumes, the index's own failure is the entire failed
// total, and comparing it against zero capacity declares a job beyond repair
// whose content downloaded completely and unpacks. That was masked for as long
// as the capacity figure counted the index too, since the comparison then
// weighed the index against itself and came out false by exact tie.
//
// Derives from per-file state rather than re-reading the manifest, so it stays
// correct after the manifest is evicted — IsPar2 was classified from the
// manifest's subject when the file was first seen, and the classification
// stays on FileProgress.
//
// There is no residency at which progress exists without having had a
// manifest to classify from, so the classification is always available.
func (p *JobProgress) ContentFailedBytes() int64 {
	if p == nil {
		return 0
	}
	var n int64
	for i := range p.files {
		if !p.files[i].IsPar2 {
			n += p.files[i].FailedBytes
		}
	}
	return n
}

// HasPar2Files reports whether the job carries any par2 file at all, index or
// recovery volume.
//
// It exists to separate two states that a recovery-bytes figure of zero cannot
// tell apart: a job with no par2 protection whatsoever, and a job whose par2
// files simply did not match the volume-naming convention. The first is a real
// finding. The second is ignorance, and acting on it as though it were a
// finding discards downloads that par2 could repair — see the guard in
// failMsgForJob and the dispatcher's Early Health Gate.
func (p *JobProgress) HasPar2Files() bool {
	if p == nil {
		return false
	}
	for i := range p.files {
		if p.files[i].IsPar2 {
			return true
		}
	}
	return false
}

// FailedBytes returns the sum of bytes belonging to permanently failed articles.
func (p *JobProgress) FailedBytes() int64 {
	if p == nil {
		return 0
	}
	return p.failedBytes
}

// RemainingBytes returns what is still to fetch, computed from per-file
// state rather than read from a maintained counter — see
// derivedRemainingBytes.
func (p *JobProgress) RemainingBytes() int64 {
	if p == nil {
		return 0
	}
	return p.derivedRemainingBytes()
}

// derivedRemainingBytes computes what is still to fetch from per-file state
// rather than from a maintained counter: every file that is neither complete
// nor deferred contributes the part of it neither downloaded nor permanently
// failed.
//
// Failed bytes are subtracted because the counter this replaces means
// unresolved bytes, not un-downloaded ones: markFailed decrements it without
// ever adding to BytesDownloaded. internal/app/history_helper.go computes
// downloaded as expectedBytes - FailedBytes() - RemainingBytes(), an identity
// that only closes under that meaning.
//
// Files the job is not fetching contribute nothing because their articles are
// never dispatched — both FetchIfNeeded and FetchNever, since neither is being
// downloaded. Holding, releasing or discarding a volume therefore needs no
// adjustment anywhere; the next read reflects it. That is the whole point of
// deriving rather than maintaining.
//
// O(files), and files number in the hundreds where articles number in the
// tens of thousands. Called on reporting reads, not on the download path.
func (p *JobProgress) derivedRemainingBytes() int64 {
	_, remaining := p.sizeFigures()
	return remaining
}

// sizeFigures walks the files once and returns the two figures that must
// agree: what the job expects to fetch, and how much of it is left.
//
// One walk rather than two because the exclusion sets are not independent.
// Both skip anything the job is not fetching (Fetch != FetchAlways, so both
// FetchIfNeeded and FetchNever); only remaining also skips Complete, because a complete
// file has nothing left to fetch while still being part of what the job set
// out to fetch. Computed apart, that relationship is a convention two
// functions have to keep by hand — and a consumer pairing figures whose
// exclusion sets have drifted gets a percentage or a downloaded total that is
// wrong in a way no test of either figure alone would catch. Here it is one
// continue-chain, so the shared half cannot drift and the divergent half is
// visible in a single place.
//
// O(files), and files number in the hundreds where articles number in the
// tens of thousands. Called on reporting reads, not on the download path.
func (p *JobProgress) sizeFigures() (expected, remaining int64) {
	if p == nil {
		return 0, 0
	}
	for fi := range p.files {
		f := &p.files[fi]
		if f.Fetch != FetchAlways {
			continue
		}
		expected += f.Bytes
		if f.Complete {
			continue
		}
		if left := f.Bytes - f.BytesDownloaded - f.FailedBytes; left > 0 {
			remaining += left
		}
	}
	return expected, remaining
}

// ExpectedBytes returns the size of what this job is expected to fetch:
// every file that has not been deferred, whether or not it has been
// downloaded yet.
//
// This is the size that must be paired with RemainingBytes; sizeFigures
// computes both from one walk so their exclusion sets cannot drift apart.
//
// It is therefore NOT Job.TotalBytes(), which is the immutable
// whole-manifest total and still includes deferred recovery volumes. See
// docs/job-lifecycle.md's byte-accounting section, which
// records that a job's advertised expectation moving as par2 decisions are
// made is a deliberate consequence.
//
// A deferred file contributes zero to all three of ExpectedBytes,
// RemainingBytes, and FailedBytes — which is what lets
// downloaded=expected-failed-remaining close. The third leg holds only
// because Deferred is never toggled on a file that already has resolved
// articles: markFailed adds to the job-level failedBytes and to the
// file's own FailedBytes unconditionally, with no check of Deferred, and
// recompute sums failedBytes over every file including
// deferred ones. Today no caller defers a file after any of its articles
// have been dispatched, so a deferred file's FailedBytes is always zero in
// practice — but that is an invariant of the callers, not of this
// function. A future change that starts deferring a partially-downloaded
// file needs to either exclude it from failedBytes accounting too, or
// accept that the identity above stops closing for that file.
func (p *JobProgress) ExpectedBytes() int64 {
	expected, _ := p.sizeFigures()
	return expected
}

// ProgressFigures returns (expected, remaining, failed) bytes in a single pass
// over the file progress entries.
//
// Computing them together under one lock acquisition prevents torn values
// where expected, remaining, or failed bytes drift across concurrent updates
// (such as article failure or on-demand par2 release).
func (p *JobProgress) ProgressFigures() (expected, remaining, failed int64) {
	if p == nil {
		return 0, 0, 0
	}
	expected, remaining = p.sizeFigures()
	return expected, remaining, p.failedBytes
}

// ServerStats returns a defensive copy, matching cloneJob's current
// maps.Copy behavior — callers cannot mutate the job's live map through it.
func (p *JobProgress) ServerStats() map[string]int64 {
	if p == nil {
		return nil
	}
	return maps.Clone(p.serverStats)
}

// DownloadStarted returns the wall-clock time the first article began downloading, or zero.
func (p *JobProgress) DownloadStarted() time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.downloadStarted
}

// DownloadFinished returns the wall-clock time the download phase completed, or zero.
func (p *JobProgress) DownloadFinished() time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.downloadFinished
}

// isJobStamp reports whether t may be stored as a download timestamp.
//
// The bound is expressed on t.Unix() rather than on t itself, and that is the
// point. SQLiteStore encodes these fields with t.Unix() and reads 0 as
// "absent". A bound of t.After(time.Unix(0, 0)) would admit the whole interval
// (epoch, epoch+1s), every member of which encodes to 0 and reads back as
// time.Time{} — the round-trip loss #464 reports, merely narrowed. Comparing on
// the encoded value makes the predicate and the wire form the same set by
// construction.
//
// It also subsumes the IsZero() test that markStartedOnce and
// markDownloadFinishedOnce used to apply: time.Time{} is year 1, whose Unix()
// is -62135596800. Both delegate here now, so the rule is one predicate rather
// than two that happened to agree.
//
// That no job timestamp is at or before the Unix epoch is a decision settled on
// #464, not something derived from the code. Every production path stamps from
// time.Now(), so a value failing this test is a programming error, not data.
func isJobStamp(t time.Time) bool { return t.Unix() > 0 }

// jobStampOrZero returns t when it is a stamp this process could have minted,
// and the zero time otherwise.
//
// It exists so the filter has one application rather than two. Restored stamps
// reach a job by two routes — restoreDownloadStamps into a live JobProgress,
// and Job.RestoreProgressState into the Job-level copy a job carries before
// hydration — and a stamp accepted by one route and rejected by the other
// would read differently from the same accessor before and after hydration.
func jobStampOrZero(t time.Time) time.Time {
	if isJobStamp(t) {
		return t
	}
	return time.Time{}
}

// setDownloadStartedOnce records the download start, reporting whether it took.
// A later call is a no-op: first start wins.
//
// This and its four siblings below are the only functions in this package's
// non-test sources that assign p.downloadStarted or p.downloadFinished by
// name. Everything else reaches the fields through them: Job.MarkJobStarted
// calls this one, and the pipeline's handleSuccessResult calls MarkJobStarted
// for every decoded article, so the first wins; Job.MarkDownloadFinished calls
// setDownloadFinishedOnce; Job.ClearDownloadFinished calls
// clearDownloadFinished; ResetForRetry calls clearDownloadStamps; and
// AttachContent and RestoreProgressState install persisted stamps through
// restoreDownloadStamps.
//
// That claim is enforced rather than cited.
// TestDownloadStampWriters_MatchTheEnumerationStatedInProse walks the package
// AST and fails when the writer set moves, per field rather than as a union —
// which a grep could not do, and which is what makes a setter miswired to
// write its sibling's field visible here. It replaced a hand-run grep that
// stated a count: the count was correct and would have gone stale silently,
// because a comment is neither compiled nor executed.
//
// Refusing a non-stamp does NOT consume the first-wins slot: a real timestamp
// arriving afterwards is still the first mark.
func (p *JobProgress) setDownloadStartedOnce(t time.Time) bool {
	// The downloadFinished test is the ordering half of the rule: a start may
	// not be recorded once a finish is. Without it a job with a finish and no
	// start — one that early-aborted before any article landed — could take a
	// start from an article still in flight that post-dates the finish, a
	// negative duration in the history record.
	// See TestSetDownloadStartedOnce_RefusesAStartAfterTheFinish.
	if !isJobStamp(t) || !p.downloadStarted.IsZero() || !p.downloadFinished.IsZero() {
		return false
	}
	p.downloadStarted = t
	return true
}

// setDownloadFinishedOnce records the download completion, reporting whether it
// took. A later call is a no-op: first finish wins. See setDownloadStartedOnce
// for the ownership rule both obey.
func (p *JobProgress) setDownloadFinishedOnce(t time.Time) bool {
	if !isJobStamp(t) || !p.downloadFinished.IsZero() {
		return false
	}
	p.downloadFinished = t
	return true
}

// clearDownloadFinished reopens the finish slot and leaves the start alone. The
// finish is the time the job last left Fetching, so a job reported back to
// Fetching gives up the one it recorded and takes a new one when it leaves
// again. It is its own door rather than a restoreDownloadStamps call, which
// installs stamps the process did not mint and is not a mark.
func (p *JobProgress) clearDownloadFinished() {
	p.downloadFinished = time.Time{}
}

// clearDownloadStamps reopens both first-wins slots. `git grep -c
// 'clearDownloadStamps()' -- '*.go' ':!*_test.go'` returns 2 lines, one per
// file: job.go, where ResetForRetry calls it because a re-download legitimately
// re-stamps, and this file, where restoreDownloadStamps below clears before
// installing.
//
// restoreDownloadStamps(time.Time{}, time.Time{}) would do the same thing, so
// this is a degenerate case of its sibling. It exists because the two are read
// at call sites that mean different things — a retry reopens slots it intends
// to re-win, a load installs slots already won in another run — and a reader of
// ResetForRetry should not have to evaluate isJobStamp(time.Time{}) to see that
// the line clears.
func (p *JobProgress) clearDownloadStamps() {
	p.downloadStarted = time.Time{}
	p.downloadFinished = time.Time{}
}

// restoreDownloadStamps installs stamps read back from persistence, bypassing
// first-wins because a restore is not a mark: the slots it fills were already
// won in the run that wrote them.
//
// It applies isJobStamp because this is the owner's entry point for values it
// did not mint. That is Rule 2, not a migration path: Rule 1 waives any duty to
// a row an earlier build wrote, and the guard is not here for one. It is here
// so that no caller can install a stamp the store cannot represent — the same
// reason the setters test it, applied at the door the setters do not cover.
//
// Via the store the guard is currently redundant, and deliberately kept:
// decodeJobStamp already yields either a positive stamp or time.Time{}. The
// redundancy is the point of a gatekeeper — it holds for the next caller too,
// and a check that only pays off when someone makes a mistake reads as dead
// code until the mistake.
//
// Callers: `git grep -c 'restoreDownloadStamps(' -- '*.go' ':!*_test.go'`
// returns 2 files — this file (the declaration, and comments naming it) and
// content.go,
// where AttachContent seeds a fresh JobProgress from the stamps
// RestoreProgressState recorded on the Job before hydration (#504), and
// RestoreProgressState installs them into a live one. It reads a stamp the
// process did not mint, which is what this method is the door for.
func (p *JobProgress) restoreDownloadStamps(started, finished time.Time) {
	p.clearDownloadStamps()
	p.downloadStarted = jobStampOrZero(started)
	p.downloadFinished = jobStampOrZero(finished)
}

// Par2Recovered reports whether on-demand par2 has un-deferred this job's recovery volumes.
func (p *JobProgress) Par2Recovered() bool {
	if p == nil {
		return false
	}
	return p.par2Recovered
}

// Par2ReleaseReason explains why deferred recovery volumes were released for download.
func (p *JobProgress) Par2ReleaseReason() string {
	if p == nil {
		return ""
	}
	return p.par2ReleaseReason
}

func (p *JobProgress) setPar2ReleaseReason(reason string) {
	p.par2ReleaseReason = reason
}

func (p *JobProgress) clearPar2ReleaseReason() {
	p.par2ReleaseReason = ""
}

func (p *JobProgress) restorePar2ReleaseReason(reason string) {
	p.par2ReleaseReason = reason
}

func (p *JobProgress) restorePar2Recovered(recovered bool) {
	p.par2Recovered = recovered
}

// HasPar2Verdict reports whether the on-demand par2 verdict has already been
// reached for this job, using the reason string as the marker.
//
// One writer sets it without a verdict: content.go's permanent-article-failure
// path. That is safe rather than an exception, and the branch is why — the
// assignment sits inside `if job.undeferRecovery(...)`, and undeferRecovery
// sets par2Recovered whenever it reports a change (`git grep -n 'func (j \*Job) undeferRecovery' internal/job/`). So
// every caller that must exclude that case already tests Par2Recovered().
//
// ResetForRetry is the only clearer (`git grep -n 'func (j \*Job) ResetForRetry' internal/job/`), which is what makes a
// retry re-derive the verdict rather than inherit it.
func (p *JobProgress) HasPar2Verdict() bool {
	if p == nil {
		return false
	}
	return p.par2ReleaseReason != ""
}

// HasDeferredPar2 reports whether any file is still held pending the CRC
// verdict. Deliberately FetchIfNeeded only: a discarded volume is not held,
// it is decided, and reporting it as held would re-run the full CRC
// verification on every subsequent completion event.
func (p *JobProgress) HasDeferredPar2() bool {
	if p == nil {
		return false
	}
	for i := range p.files {
		if p.files[i].Fetch == FetchIfNeeded {
			return true
		}
	}
	return false
}

// UsesOnDemandPar2 reports whether any file is being withheld from download
// under a non-default fetch policy — either awaiting the CRC verdict
// (FetchIfNeeded) or already ruled unnecessary (FetchNever).
//
// Distinct from HasDeferredPar2, which is FetchIfNeeded only because it gates
// re-verification. This drives the "par2 on-demand" badge, which describes
// what the job is doing rather than what it is waiting on: reported as
// HasDeferredPar2, the badge would disappear at the moment the feature
// succeeds.
func (p *JobProgress) UsesOnDemandPar2() bool {
	if p == nil {
		return false
	}
	for i := range p.files {
		if p.files[i].Fetch != FetchAlways {
			return true
		}
	}
	return false
}

// DeferredRecoveryIndices returns the file indices of recovery volumes still
// held pending the verdict.
//
// FetchIfNeeded only, and that exclusion is load-bearing rather than tidy.
// undeferRecovery walks this list on any first-time permanent article
// failure while the job is not yet par2-recovered. If a discarded volume
// appeared here, one late failure would re-activate exactly the volumes the
// CRC oracle proved unnecessary — undoing on-demand par2 entirely.
func (p *JobProgress) DeferredRecoveryIndices() []int {
	var idxs []int
	for i := range p.files {
		if p.files[i].Fetch == FetchIfNeeded {
			idxs = append(idxs, i)
		}
	}
	return idxs
}

// describesSameJobAs reports whether p was sized for a manifest of m's
// shape. It is the precondition every pairing of a live JobProgress with a
// freshly read Manifest has to satisfy, and recompute panics when it does
// not hold.
//
// This compares sizes only — NumFiles/NumArticles against
// len(p.files)/p.done.Len() — so it detects a manifest blob whose shape
// disagrees with progress, which used to happen through a torn
// Store.ReplaceManifest write and now happens only through on-disk
// corruption (the file set is immutable after Add, and ReplaceManifest is
// gone). It does NOT detect job_files rows altered out of band:
// SQLiteStore.RestoreJobProgress fills progress.files by file_index without
// resizing it, so a row deleted or renumbered outside this process still
// satisfies this size check and silently attaches its state to the wrong
// file. See ErrManifestStale for the boot-path gap (#278), which this
// guard does not cover either.
func (p *JobProgress) describesSameJobAs(m *Manifest) bool {
	if p == nil || m == nil {
		return false
	}
	return p.done.Len() == m.NumArticles() && len(p.files) == m.NumFiles()
}

// clone returns a deep copy, used by cloneJob.
func (p *JobProgress) clone() *JobProgress {
	cp := *p

	cp.done = p.done.Clone()
	cp.failed = p.failed.Clone()
	cp.emitted = p.emitted.Clone()
	cp.files = slices.Clone(p.files)

	cp.serverStats = maps.Clone(p.serverStats)
	// The map is copied and the row slices are shared: a stored slice is never
	// edited in place (see verified.go), so the clone's view cannot change.
	cp.written = maps.Clone(p.written)
	return &cp
}

// recompute recalculates each file's Pending and the job-level
// pendingArticles/articlesResolved/articlesFailed/failedBytes counters from
// the ground truth (done/failed/emitted flags), against m's file ranges.
// Called after Add and Load, and after any bulk state change
// (ClearEmittedForReload, undeferRecovery) where incremental tracking is
// impractical.
//
// recompute is authoritative for the job-level failedBytes wherever a
// manifest is resident: it derives the figure from the failed bits and the
// manifest's article sizes, so it cannot stack on an earlier value.
//
// Between recomputes, markFailed and resetForReload maintain the value
// incrementally; both take the manifest. While the manifest is evicted nothing
// maintains it: Job.MarkArticleFailed sets the bits alone, and the value lags
// them until RestoreContent recomputes at the next hydration.
func (p *JobProgress) recompute(m *Manifest) {
	// m is passed in separately from p, and nothing here ties it to the
	// manifest p was sized from (newJobProgress). A size
	// mismatch here means every article-indexed write below — markDone's
	// bitset.Set, byte accounting, pendingArticles — would
	// otherwise either silently no-op (bitset.Set/Clear are deliberately
	// lenient, see bitset.go) or run against the wrong article entirely,
	// leaving byte accounting permanently and silently wrong. Panic rather
	// than let that drift start: this mirrors the file dimension of the
	// same mismatch, which already panics loudly via the p.files[fi] index
	// below when m has more files than p was sized for.
	if p.done.Len() != m.NumArticles() {
		panic(fmt.Sprintf("job: JobProgress/Manifest article count mismatch: progress sized for %d articles, manifest has %d — they were loaded or constructed independently and never reconciled", p.done.Len(), m.NumArticles()))
	}
	total := 0
	var resolved, failed int
	var failedBytesTotal int64
	for fi := range m.NumFiles() {
		lo, hi := m.FileRange(fi)
		n := 0
		var downloaded, fileFailed int64
		// Files that are not being fetched (on-demand par2 recovery volumes,
		// held or discarded) are never dispatched, so they contribute zero
		// pending work.
		fetching := p.files[fi].Fetch == FetchAlways
		for i := lo; i < hi; i++ {
			if fetching && !p.done.Get(i) && !p.emitted.Get(i) {
				n++
			}
			if p.done.Get(i) && !p.failed.Get(i) {
				downloaded += int64(m.ArticleBytes(i))
			}
			if p.done.Get(i) {
				resolved++
				if p.failed.Get(i) {
					failed++
					fileFailed += int64(m.ArticleBytes(i))
				}
			}
		}
		p.files[fi].Pending = n
		p.files[fi].BytesDownloaded = downloaded
		p.files[fi].FailedBytes = fileFailed
		// Bytes is ground truth held by the manifest; taking it from m keeps
		// derivedRemainingBytes in the manifest's terms.
		p.files[fi].Bytes = m.FileBytes(fi)
		failedBytesTotal += fileFailed
		total += n
	}
	p.pendingArticles = total
	p.articlesResolved = resolved
	p.articlesFailed = failed
	p.failedBytes = failedBytesTotal
}

// markEmitted flags article i as having a result in flight from the
// downloader to the assembler. Idempotent: a no-op if the article is
// already Emitted, Done, or Failed.
func (p *JobProgress) markEmitted(m *Manifest, i int) {
	if p.emitted.Get(i) || p.done.Get(i) {
		return
	}
	p.emitted.Set(i)
	fi := m.fileIndexForArticle(i)
	p.files[fi].Pending--
	p.pendingArticles--
}

// clearEmitted resets the transient Emitted flag on article i, restoring it
// to pending unless it has already completed.
func (p *JobProgress) clearEmitted(m *Manifest, i int) {
	if p.emitted.Get(i) && !p.done.Get(i) {
		p.emitted.Clear(i)
		fi := m.fileIndexForArticle(i)
		p.files[fi].Pending++
		p.pendingArticles++
	} else if p.emitted.Get(i) {
		p.emitted.Clear(i)
	}
}

// markDone flips Done on article i and updates counters. Returns false
// (no-op) if the article was already Done.
//
//nolint:unparam // bool return is part of JobProgress API and used in tests
func (p *JobProgress) markDone(m *Manifest, i int) bool {
	if p.done.Get(i) {
		return false
	}
	fi := m.fileIndexForArticle(i)
	if !p.emitted.Get(i) {
		p.files[fi].Pending--
		p.pendingArticles--
	}
	p.done.Set(i)
	p.emitted.Clear(i)
	bytes := int64(m.ArticleBytes(i))
	p.articlesResolved++
	p.files[fi].BytesDownloaded += bytes
	return true
}

// markNotDone returns article i to Outstanding. It is the inverse of markDone,
// and it has exactly one caller: Job.UntrustFile, which returns a file's
// articles to Outstanding once its fsync failed — `git grep -n -E '\.markNotDone[(]' -- '*.go' ':!*_test.go'`
// finds 1 line, in verified.go. Nothing on the download path may call it — an
// ack is a one-way transition (R9).
//
// It clears the bit and nothing else. The figures markDone maintains are
// deliberately NOT unwound here article by article: JobProgress.recompute
// already derives every one of them from the bitmaps, and it applies rules a
// per-article inverse would have to reproduce by hand — Pending counts only
// files whose Fetch is FetchAlways, and only articles that are neither done
// nor emitted. A copy of those rules that drifts is a half-inverse, and a
// half-inverse of markDone is how #300 arose from the other direction: bits
// and derived figures disagreeing, so the job reports a health its per-article
// state does not support. UntrustFile recomputes once for the whole file
// instead.
//
// A permanently failed article is never cleared, and that is a rule about what
// the caller's evidence covers rather than an optimisation. failed implies
// done, but a failed article's bytes were never written, so their absence from
// the file is not new information — it is the recorded outcome. Clearing it
// would re-fetch the article on every restart, return its bytes to the
// job's health figures as if they might still arrive, and burn its retry
// budget over a fact already established (R10, R21).
//
// Returns false when it changed nothing: the article was already Outstanding,
// or it is permanently failed.
func (p *JobProgress) markNotDone(i int) bool {
	if !p.done.Get(i) || p.failed.Get(i) {
		return false
	}
	p.done.Clear(i)
	return true
}

// markFailed flips Done+Failed on article i and updates counters. Returns
// false (no-op) if the article was already Done.
func (p *JobProgress) markFailed(m *Manifest, i int) bool {
	wasEmitted := p.emitted.Get(i)
	if !p.setFailedBits(i) {
		return false
	}
	fi := m.fileIndexForArticle(i)
	if !wasEmitted {
		p.files[fi].Pending--
		p.pendingArticles--
	}
	bytes := int64(m.ArticleBytes(i))
	p.failedBytes += bytes
	p.files[fi].FailedBytes += bytes
	p.articlesResolved++
	p.articlesFailed++
	return true
}

// setFailedBits is the bit transition of a permanent failure: Done and Failed
// set, Emitted cleared. Returns false (no-op) if the article was already Done.
//
// It needs no manifest and maintains no counter. markFailed calls it and then
// maintains the counters itself; Job.MarkArticleFailed calls it alone for a job
// whose manifest is evicted, leaving the counters to the recompute in
// RestoreContent.
func (p *JobProgress) setFailedBits(i int) bool {
	if p.done.Get(i) {
		return false
	}
	p.done.Set(i)
	p.failed.Set(i)
	p.emitted.Clear(i)
	return true
}

// resetForReload clears the transient Emitted flag on article i and, if it
// was Failed and its file is still open for writing, resets it to retryable
// (Done=false, Failed=false), subtracting its bytes from FailedBytes.
// RemainingBytes needs no restoring of its own: it derives from
// BytesDownloaded/FailedBytes on read, and an article that was never
// downloaded leaves BytesDownloaded untouched, so undoing FailedBytes here is
// what makes the article's bytes reappear as remaining. Used by
// ClearEmittedForReload on a downloader reload; recompute must be called afterward
// to rebuild Pending counters from the resulting ground truth.
//
// # Why a Complete file's article is left resolved (#426)
//
// A failed article whose file is already Complete stays Done and Failed. The
// combination is ordinary rather than corrupt: a permanently failed article
// keeps its place in the file's part total, so the file still reaches
// TotalParts, finalizes short, and is marked Complete.
//
// Resetting it would create work nothing can perform. ForEachUnfinishedArticle
// skips a Complete file outright, so the article would be counted in Pending
// and never dispatched — the job reports outstanding work forever. Clearing
// Complete instead does not help: the file is finalized, truncated to its
// durable bound, and in the assembler's completed set, which is never cleared
// for the life of the process, so a re-fetched article routes to
// handleLateDuplicate, its buffer is pooled, nothing is written, and it is
// failed again. The reset would buy one wasted fetch per article and arrive
// back here.
//
// Keeping the bytes is the honest figure for the same reason: the article
// really did fail, and unwinding FailedBytes on a file that shipped short
// would understate the damage par2 is being asked to repair.
//
// The narrower guard reloader.go rejects — skipping an article the writer
// still holds — is a different one, and its objection does not apply here. It
// needs knowledge of the writer that this layer does not have; Complete is
// queue-owned state.
// It reports whether it cleared article i's failed bit, which
// ClearEmittedForReload aggregates into its `cleared` return. NO CALLER USES
// THAT RETURN TODAY — `git grep -n 'j\.ClearEmittedForReload(' -- '*.go'
// ':!*_test.go'` finds 1 line, and discards it. The per-article answer
// exists so that a caller CAN name the stored rows it may drop: now that the
// reset is not exhaustive, a whole-job delete would forget an article that is
// still failed in memory.
// clearEmitted is false when the caller passes skipEmitted=true to
// ClearEmittedForReload, withholding the Emitted clear (#417). The production
// caller, internal/app/reloader.go, passes false, so clearEmitted is true
// there; the `j\.ClearEmittedForReload(` grep cited above is the enumeration
// of production callers. Only the
// Emitted clear is withheld — the un-failing below still
// runs, because the two act on DISJOINT article sets and
// withholding both would trade one permanent strand for another.
//
// The disjointness, since it is what makes the narrow skip correct: markFailed
// CLEARS emitted as it sets failed, and the un-fail arm below is gated on
// p.failed.Get(i). So a Failed article is never Emitted, and #417's strand is
// caused only by the unconditional emitted.Clear on an article that is
// emitted-and-not-done. Skipping the un-fail instead would leave an article the
// old downloader's teardown failed — ErrNoServersLeft is terminal — failed
// for the rest of the process: markNotDone refuses a permanently failed
// article, and only a whole-job retry clears it.
func (p *JobProgress) resetForReload(m *Manifest, i int, clearEmitted bool) bool {
	if clearEmitted {
		p.emitted.Clear(i)
	}
	if !p.failed.Get(i) {
		return false
	}
	fi := m.fileIndexForArticle(i)
	if p.files[fi].Complete {
		return false
	}
	bytes := int64(m.ArticleBytes(i))
	p.failedBytes -= bytes
	p.files[fi].FailedBytes -= bytes
	p.done.Clear(i)
	p.failed.Clear(i)
	return true
}

// isEarlyAbort returns true if the job should be aborted based on the
// first-article failure rate. See Job.IsEarlyAbort for the heuristic.
func (p *JobProgress) isEarlyAbort() bool {
	if p.earlyAborted {
		return false
	}
	if p.articlesResolved < earlyAbortSample {
		return false
	}
	rate := float64(p.articlesFailed) / float64(p.articlesResolved)
	if rate >= earlyAbortThreshold {
		p.earlyAborted = true
		return true
	}
	return false
}
