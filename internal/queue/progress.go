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

// FileProgress is one file's mutable assembly state.
type FileProgress struct {
	Complete bool
	Deferred bool
	Pending  int
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
	FailedBytes    int64
	WriteCursor    int64
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
		files[fi] = FileMeta{ArticleCount: hi - lo, Bytes: m.FileBytes(fi)}
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

// FileDeferred reports whether file fileIdx is currently held back from dispatch.
func (p *JobProgress) FileDeferred(fi int) bool {
	if p == nil || fi < 0 || fi >= len(p.files) {
		return false
	}
	return p.files[fi].Deferred
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
// downloaded as totalBytes - FailedBytes() - RemainingBytes(), an identity
// that only closes under that meaning.
//
// Deferred files contribute nothing because their articles are never
// dispatched, so a deferral or an un-deferral needs no adjustment anywhere —
// the next read reflects it. That is the whole point of deriving rather than
// maintaining.
//
// O(files), and files number in the hundreds where articles number in the
// tens of thousands. Called on reporting reads, not on the download path.
func (p *JobProgress) derivedRemainingBytes() int64 {
	var remaining int64
	for fi := range p.files {
		f := &p.files[fi]
		if f.Complete || f.Deferred {
			continue
		}
		if left := f.Bytes - f.BytesDownloaded - f.FailedBytes; left > 0 {
			remaining += left
		}
	}
	return remaining
}

// ExpectedBytes returns the size of what this job is expected to fetch:
// every file that has not been deferred, whether or not it has been
// downloaded yet.
//
// This is the size that must be paired with RemainingBytes. The two share
// a walk and a predicate on purpose — a consumer computing a percentage or
// a downloaded total from figures with different exclusion sets gets a
// number that is wrong in a way no test of either figure alone would
// catch. RemainingBytes additionally skips Complete files, because they
// have nothing left to fetch; ExpectedBytes counts them, because they are
// part of what the job set out to fetch.
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
	if p == nil {
		return 0
	}
	var expected int64
	for fi := range p.files {
		if p.files[fi].Deferred {
			continue
		}
		expected += p.files[fi].Bytes
	}
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

// HasDeferredPar2 reports whether any file is currently deferred.
func (p *JobProgress) HasDeferredPar2() bool {
	if p == nil {
		return false
	}
	for i := range p.files {
		if p.files[i].Deferred {
			return true
		}
	}
	return false
}

// DeferredRecoveryIndices returns the file indices of all currently-deferred
// par2 recovery volumes.
func (p *JobProgress) DeferredRecoveryIndices() []int {
	var idxs []int
	for i := range p.files {
		if p.files[i].Deferred {
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
// The two can genuinely disagree. DiscardDeferredPar2 rebuilds a smaller
// manifest and progress in memory, and persists both through
// Store.ReplaceManifest — a blob write plus a transaction that cannot be made
// atomic together (see ErrManifestStale). A crash between them leaves the file
// on disk describing the job as it was before the discard. Re-reading it and
// pairing it with the surviving, smaller progress is not a recoverable state:
// the manifest is simply not this job's any more.
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
// (ClearAllEmitted, DiscardDeferredPar2, undeferRecoveryLocked) where
// incremental tracking is impractical.
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
		// Deferred files (on-demand par2 recovery volumes) are not dispatched,
		// so they contribute zero pending work.
		deferred := p.files[fi].Deferred
		for i := lo; i < hi; i++ {
			if !deferred && !p.done.Get(i) && !p.emitted.Get(i) {
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
	Complete       bool   `json:"complete,omitempty"`
	Deferred       bool   `json:"deferred,omitempty"`
	WriteCursor    int64  `json:"write_cursor,omitempty"`
	Filename       string `json:"filename,omitempty"`
	AssembledCRC32 uint32 `json:"assembled_crc32,omitempty"`
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
			Deferred:       f.Deferred,
			WriteCursor:    f.WriteCursor,
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
			Deferred:       f.Deferred,
			WriteCursor:    f.WriteCursor,
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
