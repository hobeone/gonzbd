package queue

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"
)

// JobProgress is the mutable per-article and per-file state of a job:
// which articles are done/failed/emitted, per-file assembly bookkeeping,
// and job-level counters. Always sized to match its Manifest's
// NumArticles()/NumFiles(). Deep-copied on every Snapshot/SnapshotJob
// (unlike Manifest, which is shared).
type JobProgress struct {
	// Flat, global article index. emitted is transient and never persisted.
	// Bitsets rather than []bool: see bitset.go for the memory argument.
	done, failed, emitted bitset
	files                 []FileProgress

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
	// Bytes is the file's NZB-claimed size. Carried on progress rather than
	// read from the manifest so RemainingBytes derives from progress alone,
	// at any residency. Written from the manifest when resident and from
	// job_files.bytes when not; the two agree because the column is written
	// from the manifest.
	Bytes           int64
	BytesDownloaded int64
	// FailedBytes is the sum of bytes belonging to this file's permanently
	// failed articles. Carried per file, not just job-wide, so remaining
	// derives from progress alone at any residency: a failed article was
	// never downloaded, so BytesDownloaded does not account for it, and
	// without this the derivation would report its bytes as still to fetch
	// forever. Recomputable from the article bitmaps when a manifest is
	// resident; persisted in job_files.failed_bytes for when it is not.
	FailedBytes int64
	// IsPar2 marks a par2 file — the index or a recovery volume — as opposed
	// to content. Carried per file, like Bytes and FailedBytes, so
	// ContentFailedBytes derives from progress alone at any residency.
	//
	// Note this is broader than job_files.is_par2_recovery, which flags only
	// volumes. The index matters here precisely because it is not a recovery
	// volume: it is fetched, so it can fail, and its failure is not damage.
	// Both construction paths classify by subject rather than reading a
	// column, which is why no schema change is needed — job_files already
	// stores the subject.
	IsPar2      bool
	WriteCursor int64
	// MaxWritten is the highest byte position the assembler has written for
	// this file — its decoded high-water mark. Persisted so a resumed run
	// starts from the file's real extent instead of zero; without it the
	// completion truncate cuts away whatever an earlier run wrote above the
	// articles this run happens to receive (#342).
	//
	// Distinct from WriteCursor, which is the *contiguous* frontier and so
	// lags this figure whenever articles arrive out of order.
	MaxWritten     int64
	Filename       string // resolved on-disk filename; empty until resolved
	AssembledCRC32 uint32
}

// fileMetaFromManifest projects m into the same per-file shape
// Store.ArticleCountsByJob returns, so newJobProgress and
// newJobProgressSized are one code path rather than two that must be kept
// in agreement by hand. A fresh job has nothing downloaded, nothing
// failed, and no file complete or deferred, so every field but the sizes
// is zero.
//
// The projection is lossless for what JobProgress needs: Manifest.TotalBytes
// is the sum of every file's bytes, and Manifest.NumArticles is the sum of
// every file's article count, so the totals newJobProgressSized derives
// match the ones newJobProgress used to take from the manifest directly.
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

// newJobProgress returns a zero-value JobProgress sized to m: every file's
// Pending starts at its article count (all articles start undone/unemitted),
// so RemainingBytes() — derived from per-file state, see
// derivedRemainingBytes — starts at m.TotalBytes(). It projects m into
// []FileMeta and delegates to newJobProgressSized, so resident and
// non-resident construction are literally the same code and cannot drift
// apart the way the two used to (see TestFailedBytes_NotDoubledByHydration
// for what that drift cost). One side effect of delegating: pendingArticles
// is now set to m.NumArticles() here too, where it used to be left at 0
// until the first recompute — see the caller-visibility check this was
// verified against.
func newJobProgress(m *Manifest) *JobProgress {
	return newJobProgressSized(fileMetaFromManifest(m))
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

// FileWriteCursor returns the assembler's contiguous write frontier for file fileIdx.
func (p *JobProgress) FileWriteCursor(fi int) int64 {
	if p == nil || fi < 0 || fi >= len(p.files) {
		return 0
	}
	return p.files[fi].WriteCursor
}

// FileMaxWritten returns the highest byte position written for file fileIdx —
// the file's decoded high-water mark, used to seed a resumed run so the
// completion truncate does not cut below what earlier runs already wrote.
func (p *JobProgress) FileMaxWritten(fi int) int64 {
	if p == nil || fi < 0 || fi >= len(p.files) {
		return 0
	}
	return p.files[fi].MaxWritten
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
// Derives from per-file state, so it is correct at any residency: IsPar2 is
// classified from the manifest when resident and from job_files.subject when
// not.
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
// docs/superpowers/specs/2026-08-05-job-size-figures-design.md, which
// records that a job's advertised expectation moving as par2 decisions are
// made is a deliberate consequence.
//
// A deferred file contributes zero to all three of ExpectedBytes,
// RemainingBytes, and FailedBytes — which is what lets
// downloaded=expected-failed-remaining close. The third leg holds only
// because Deferred is never toggled on a file that already has resolved
// articles: markFailed adds to the job-level failedBytes and to the
// file's own FailedBytes unconditionally, with no check of Deferred, and
// newJobProgressSized/recompute sum failedBytes over every file including
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

// DeferredRecoveryIndices returns the file indices of recovery volumes still
// held pending the verdict.
//
// FetchIfNeeded only, and that exclusion is load-bearing rather than tidy.
// undeferRecoveryLocked walks this list on any first-time permanent article
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
	return &cp
}

// recompute recalculates each file's Pending and the job-level
// pendingArticles/articlesResolved/articlesFailed/failedBytes counters from
// the ground truth (done/failed/emitted flags), against m's file ranges.
// Called after Add and Load, and after any bulk state change
// (ClearAllEmitted, undeferRecoveryLocked) where incremental tracking is
// impractical.
//
// recompute is authoritative for the job-level failedBytes wherever a
// manifest is resident: RestoreJobProgress replays per-article state
// through markFailed on top of a progress that newJobProgressSized may
// already have seeded from job_files, so without a single owner the seed
// and the replay stack (see TestFailedBytes_NotDoubledByHydration).
// Incremental maintenance by markFailed/resetForReload is what carries the
// value between recomputes, while no manifest is resident to recompute
// against.
func (p *JobProgress) recompute(m *Manifest) {
	// JobProgress and Manifest are persisted as independent JSON documents
	// (Job.UnmarshalJSON assigns both from separate keys with nothing
	// reconciling their lengths) and independent SQLite rows. A size
	// mismatch here means every article-indexed write below — markDone's
	// bitset.Set, byte accounting, pendingArticles — would
	// otherwise either silently no-op (bitset.Set/Clear are deliberately
	// lenient, see bitset.go) or run against the wrong article entirely,
	// leaving byte accounting permanently and silently wrong. Panic rather
	// than let that drift start: this mirrors the file dimension of the
	// same mismatch, which already panics loudly via the p.files[fi] index
	// below when m has more files than p was sized for.
	if p.done.Len() != m.NumArticles() {
		panic(fmt.Sprintf("queue: JobProgress/Manifest article count mismatch: progress sized for %d articles, manifest has %d — they were loaded or constructed independently and never reconciled", p.done.Len(), m.NumArticles()))
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
		// Bytes is deliberately not part of jobProgressJSON (see
		// fileProgressJSON) — it is ground truth already held by the
		// manifest, so it is restored here rather than carried over the
		// wire a second time. Without this, a JobProgress rebuilt via
		// UnmarshalJSON+recompute would leave every file's Bytes at its
		// zero value and derivedRemainingBytes would report everything as
		// already fetched.
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

// markFailed flips Done+Failed on article i and updates counters. Returns
// false (no-op) if the article was already Done.
func (p *JobProgress) markFailed(m *Manifest, i int) bool {
	if p.done.Get(i) {
		return false
	}
	fi := m.fileIndexForArticle(i)
	if !p.emitted.Get(i) {
		p.files[fi].Pending--
		p.pendingArticles--
	}
	p.done.Set(i)
	p.failed.Set(i)
	p.emitted.Clear(i)
	bytes := int64(m.ArticleBytes(i))
	p.failedBytes += bytes
	p.files[fi].FailedBytes += bytes
	p.articlesResolved++
	p.articlesFailed++
	return true
}

// resetForReload clears the transient Emitted flag on article i and, if it
// was Failed, resets it to retryable (Done=false, Failed=false), restoring
// its bytes to FailedBytes. RemainingBytes needs no restoring of its own:
// it derives from BytesDownloaded/FailedBytes on read, and an article that
// was never downloaded leaves BytesDownloaded untouched, so undoing
// FailedBytes here is what makes the article's bytes reappear as
// remaining. Used by ClearAllEmitted on a downloader reload; recompute
// must be called afterward to rebuild Pending counters from the resulting
// ground truth.
func (p *JobProgress) resetForReload(m *Manifest, i int) {
	p.emitted.Clear(i)
	if p.failed.Get(i) {
		fi := m.fileIndexForArticle(i)
		bytes := int64(m.ArticleBytes(i))
		p.failedBytes -= bytes
		p.files[fi].FailedBytes -= bytes
		p.done.Clear(i)
		p.failed.Clear(i)
	}
}

// fileProgressJSON is one file's on-disk shape. Pending and BytesDownloaded
// are excluded — derived state, recomputed by recompute() after load.
type fileProgressJSON struct {
	Complete       bool        `json:"complete,omitempty"`
	Fetch          FetchPolicy `json:"fetch_policy,omitempty"`
	WriteCursor    int64       `json:"write_cursor,omitempty"`
	MaxWritten     int64       `json:"max_written,omitempty"`
	Filename       string      `json:"filename,omitempty"`
	AssembledCRC32 uint32      `json:"assembled_crc32,omitempty"`
}

// jobProgressJSON is JobProgress's on-disk shape. emitted, pendingArticles,
// articlesResolved, articlesFailed, and earlyAborted are all deliberately
// excluded — these are correctness exclusions, not wire-compatibility ones.
// emitted in particular must never survive a restart: on crash recovery,
// any article whose bytes hadn't reached stable storage needs to be
// re-dispatched, and persisting emitted would let it be silently skipped.
type jobProgressJSON struct {
	Done   []bool             `json:"done"`
	Failed []bool             `json:"failed"`
	Files  []fileProgressJSON `json:"files"`

	FailedBytes       int64            `json:"failed_bytes"`
	ServerStats       map[string]int64 `json:"server_stats,omitempty"`
	DownloadStarted   time.Time        `json:"download_started"`
	DownloadFinished  time.Time        `json:"download_finished"`
	Par2Recovered     bool             `json:"par2_recovered,omitempty"`
	Par2ReleaseReason string           `json:"par2_release_reason,omitempty"`
}

// MarshalJSON implements json.Marshaler.
func (p *JobProgress) MarshalJSON() ([]byte, error) {
	files := make([]fileProgressJSON, len(p.files))
	for fi, f := range p.files {
		files[fi] = fileProgressJSON{
			Complete:       f.Complete,
			Fetch:          f.Fetch,
			WriteCursor:    f.WriteCursor,
			MaxWritten:     f.MaxWritten,
			Filename:       f.Filename,
			AssembledCRC32: f.AssembledCRC32,
		}
	}
	return json.Marshal(jobProgressJSON{
		Done:              p.done.ToBools(),
		Failed:            p.failed.ToBools(),
		Files:             files,
		FailedBytes:       p.failedBytes,
		ServerStats:       p.serverStats,
		DownloadStarted:   p.downloadStarted,
		DownloadFinished:  p.downloadFinished,
		Par2Recovered:     p.par2Recovered,
		Par2ReleaseReason: p.par2ReleaseReason,
	})
}

// UnmarshalJSON implements json.Unmarshaler. pendingArticles/
// articlesResolved/articlesFailed are left zero; a caller must invoke
// recompute afterward to rebuild them from done/failed/emitted ground
// truth. Reached only through Job.UnmarshalJSON, which since #298 has no
// production caller of its own.
func (p *JobProgress) UnmarshalJSON(data []byte) error {
	var pj jobProgressJSON
	if err := json.Unmarshal(data, &pj); err != nil {
		return err
	}
	p.done = bitsetFromBools(pj.Done)
	p.failed = bitsetFromBools(pj.Failed)
	p.emitted = newBitset(len(pj.Done))
	p.files = make([]FileProgress, len(pj.Files))
	for fi, f := range pj.Files {
		p.files[fi] = FileProgress{
			Complete:       f.Complete,
			Fetch:          f.Fetch,
			WriteCursor:    f.WriteCursor,
			MaxWritten:     f.MaxWritten,
			Filename:       f.Filename,
			AssembledCRC32: f.AssembledCRC32,
		}
	}
	p.failedBytes = pj.FailedBytes
	p.serverStats = pj.ServerStats
	p.downloadStarted = pj.DownloadStarted
	p.downloadFinished = pj.DownloadFinished
	p.par2Recovered = pj.Par2Recovered
	p.par2ReleaseReason = pj.Par2ReleaseReason
	return nil
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
