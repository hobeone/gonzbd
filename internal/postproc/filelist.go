package postproc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/humanfmt"
	"github.com/hobeone/gonzbd/internal/job"
)

// buildDownloadFileList creates the lines for the synthetic "download"
// StageLogEntry. It scans job.DownloadDir (non-recursively) and produces
// a human-readable listing that includes:
//   - per-file name and size
//   - download duration and total size
//   - article completion stats from the queue
func buildDownloadFileList(j *Job) []string {
	var lines []string
	p := j.Progress()

	// The listing is a per-file report, so it needs the manifest's file
	// table and there is no scalar substitute. Degrade rather than fail:
	// this builds the history record's "download" stage lines, which is
	// reporting — the download itself is unaffected. Say so in the record
	// instead of returning a silently empty listing, since an empty file
	// list and a job that downloaded nothing look identical to a reader.
	m, mErr := j.Manifest()
	if mErr != nil {
		return []string{fmt.Sprintf("File listing unavailable: %v", mErr)}
	}

	// On-demand par2 stats: count the skipped recovery volumes. Only the
	// counts are walked here — the byte figure comes from JobProgress below.
	var heldVols, recoveryVols int
	for fi := range m.NumFiles() {
		if !m.FileIsPar2Recovery(fi) {
			continue
		}
		recoveryVols++
		if p.FileFetchPolicy(fi) != job.FetchAlways {
			heldVols++
		}
	}

	// The bytes this job held back, derived rather than summed here. Summing
	// them locally was a second implementation of the figure JobProgress
	// already derives, and the two drifted: this listing divided by the
	// manifest total while the history record divided by ExpectedBytes, so
	// the same job reported two different failure percentages (#326).
	//
	// The two expressions are not the same predicate. This one covers every
	// file that is not FetchAlways; the loop above counts only recovery
	// volumes. They agree because nothing but a recovery volume is ever moved
	// off FetchAlways — a property of the callers (see Job.ResetForRetry and
	// newJobProgress), not of JobProgress. If that ever changes, heldVols and
	// heldBytes stop describing the same set and the line printing both goes
	// wrong before anything else does.
	heldBytes := m.TotalBytes() - p.ExpectedBytes()

	// Download duration header.
	var dlDuration time.Duration
	if !p.DownloadStarted().IsZero() && !p.DownloadFinished().IsZero() {
		dlDuration = p.DownloadFinished().Sub(p.DownloadStarted())
	}

	if heldBytes > 0 {
		lines = append(lines, fmt.Sprintf("Downloaded %s (saved %s by not downloading par2 files) in %s",
			humanfmt.BytesSI(p.ExpectedBytes()),
			humanfmt.BytesSI(heldBytes),
			humanfmt.Duration(dlDuration)))
	} else {
		lines = append(lines, fmt.Sprintf("Downloaded %s in %s",
			humanfmt.BytesSI(p.ExpectedBytes()),
			humanfmt.Duration(dlDuration)))
	}

	// Failed/remaining bytes summary. The denominator is ExpectedBytes, the
	// same one the history record's completeness figure uses, so the two
	// cannot report different failure percentages for one job (#326).
	//
	// ExpectedBytes can be zero where the manifest total could not (a job
	// holding back every file). Nothing divides by it here, because this
	// branch requires failed bytes and nothing can fail without having been
	// dispatched — but that is an inherited invariant, not a check.
	if p.FailedBytes() > 0 {
		lines = append(lines, fmt.Sprintf("⚠ %s failed (%.1f%%)",
			humanfmt.BytesSI(p.FailedBytes()),
			float64(p.FailedBytes())/float64(p.ExpectedBytes())*100))
	}

	switch {
	case heldVols > 0 && !p.Par2Recovered() && p.HasPar2Verdict():
		// A verdict was reached (HasPar2Verdict) but the volumes were never
		// un-deferred (!Par2Recovered) and are still held (heldVols > 0).
		// That combination covers two situations this method cannot tell
		// apart: outcomeUnknown (nothing on disk could be identified against
		// the par2 index, so verification never ran) and an outcomeRepair
		// verdict whose UndeferRecoveryVolumes call failed (verification DID
		// find damage, but the volumes could not be fetched). Reporting
		// either as "verified clean" would be the mislabel this case exists
		// to remove, so both get the same non-committal headline; the
		// release reason in the parenthetical is what carries the real
		// explanation for whichever one actually happened.
		// HasPar2Verdict() is true in this arm (the switch guards on it
		// directly), and HasPar2Verdict is defined as Par2ReleaseReason() !=
		// "" (internal/job/progress.go:708), so the reason is always
		// non-empty here — no fallback needed.
		reasonStr := fmt.Sprintf(" (reason: %s)", p.Par2ReleaseReason())
		lines = append(lines, fmt.Sprintf("⚠ Par2: could not verify — %d recovery volume(s) held%s",
			heldVols, reasonStr))
	case heldVols > 0:
		// Still withheld at finalize. This arm is reached three ways, and
		// "only when HasPar2Verdict() is false" is NOT one of them — that
		// phrasing was tried and is false:
		//
		//  (a) on-demand par2 never ran (fetch policy off, or the job
		//      finalized before verification) — HasPar2Verdict() is false.
		//  (b) the clean-verdict path discarded the volumes without
		//      recording a reason (outcomeClean never calls
		//      SetPar2ReleaseReason) — HasPar2Verdict() is false.
		//  (c) a verdict-bearing job whose recovery volumes were only
		//      PARTIALLY un-deferred: Par2Recovered() is true (set on any
		//      change by undeferRecovery), so the case above's
		//      !p.Par2Recovered() conjunct excludes it, and some volumes
		//      are still held — HasPar2Verdict() is true.
		//
		// (c) reports "verified clean" for a job a verdict already found
		// damage in, which is the same class of mislabel the case above
		// exists to remove — reordering the switch to fix it is deliberately
		// NOT done here (tracked as issue #505; moving the `recoveryVols > 0
		// && p.Par2Recovered()` case above this one and re-running `go test
		// ./...` broke nothing in this suite, so no test currently pins the
		// order — the fix is deferred on scope, not on test coupling). It is
		// not reached today: undeferRecovery is the only site that sets
		// par2Recovered to true (internal/job/content.go:288; the
		// other assignment, content.go:605, sets it back to false on retry, and
		// the rest are the field declaration and plain reads/copies — see a
		// grep for par2Recovered outside tests), and undeferRecovery has two
		// production callers: Job.UndeferRecoveryVolumes
		// (internal/job/content.go:296, reached from app.go's
		// maybeReleaseRecoveryVolumes) and content.go's AckPermanentFailure
		// (internal/job/content.go:142). Both pass
		// job.progress.DeferredRecoveryIndices() — every deferred index at
		// once — so a real job's held volumes go from "all deferred" to
		// "none deferred" in one step and heldVols is 0 by the time
		// Par2Recovered() is true. (c) only becomes reachable once a caller
		// un-defers a strict subset — e.g. a future block-exact selection
		// seam on UndeferRecoveryVolumes' fileIdxs parameter.
		//
		// (a) and (b) both leave the bytes never fetched, which is what
		// this line reports; (c) is the latent exception.
		lines = append(lines, fmt.Sprintf("✓ Par2: verified clean from index — %d recovery volume(s) skipped (saved %s)",
			heldVols, humanfmt.BytesSI(heldBytes)))
	case recoveryVols > 0 && p.Par2Recovered():
		// Volumes were un-deferred and fetched because repair was needed.
		reasonStr := ""
		if p.Par2ReleaseReason() != "" {
			reasonStr = fmt.Sprintf(" (reason: %s)", p.Par2ReleaseReason())
		}
		lines = append(lines, fmt.Sprintf("⚠ Par2: fetched %d recovery volume(s) for repair%s", recoveryVols, reasonStr))
	}

	// Per-file download completion. Lets the user see exactly which files
	// came up short and reconcile them against the failed-bytes total above
	// (at post-processing time nothing is still pending, so any file below
	// 100% is short because some of its articles failed). Percentages are
	// derived from article byte sums rather than JobFile.Bytes so they are
	// internally consistent: a file reads 100% only when every article
	// downloaded successfully.
	lines = append(lines, buildFileCompletionLines(j)...)

	// Enumerate files in the download directory, recursing into
	// subdirectories so nothing nested (e.g. by extraction or par2 staging)
	// is hidden from the listing.
	treeLines, fileCount, err := buildDirTree(j.DownloadDir, "  ")
	if err != nil {
		lines = append(lines, fmt.Sprintf("Error reading download dir: %v", err))
		return lines
	}

	lines = append(lines, fmt.Sprintf("Files in download directory (%d):", fileCount))
	lines = append(lines, treeLines...)

	// Server stats, if available.
	if stats := p.ServerStats(); len(stats) > 0 {
		var parts []string
		for srv, bytes := range stats {
			parts = append(parts, fmt.Sprintf("%s: %s", srv, humanfmt.BytesSI(bytes)))
		}
		lines = append(lines, "Servers: "+strings.Join(parts, ", "))
	}

	return lines
}

// buildDirTree recursively lists dir's contents as indented tree lines:
// directories print as "<indent>📁 name/" with their contents indented two
// more spaces underneath; files print as "<indent>name (size)". It returns
// the lines, the total number of files found (directories are not counted),
// and an error if the top-level directory itself can't be read. Errors
// reading a subdirectory are reported inline rather than aborting the walk.
func buildDirTree(dir, indent string) (lines []string, count int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}

	for _, e := range entries {
		if e.IsDir() {
			lines = append(lines, fmt.Sprintf("%s📁 %s/", indent, e.Name()))
			subLines, subCount, subErr := buildDirTree(filepath.Join(dir, e.Name()), indent+"  ")
			if subErr != nil {
				lines = append(lines, fmt.Sprintf("%s  Error reading dir: %v", indent, subErr))
				continue
			}
			lines = append(lines, subLines...)
			count += subCount
			continue
		}
		info, _ := e.Info()
		var sz int64
		if info != nil {
			sz = info.Size()
		}
		lines = append(lines, fmt.Sprintf("%s%s (%s)", indent, e.Name(), humanfmt.BytesSI(sz)))
		count++
	}
	return lines, count, nil
}

// buildFileCompletionLines produces one line per job file showing its
// download completeness, plus a header summarising how many files are
// incomplete. Complete files are marked "✓ … 100%"; short files are marked
// "⚠ … N% (X of Y received)" so it is clear which files failed to download
// in full. A short file is not necessarily a broken file — a later par2
// repair stage may still recover it; this section reports download
// completeness only.
func buildFileCompletionLines(j *Job) []string {
	// Same reasoning as buildDownloadFileList, which has already reported
	// the cause in the record by the time this runs — no second notice.
	m, mErr := j.Manifest()
	if mErr != nil {
		return nil
	}
	p := j.Progress()
	numFiles := m.NumFiles()
	if numFiles == 0 {
		return nil
	}

	incomplete := 0
	fileLines := make([]string, 0, numFiles)
	for fi := range numFiles {
		name := m.FileSubject(fi)
		if fn := p.FileFilename(fi); fn != "" {
			name = fn
		}
		if p.FileFetchPolicy(fi) != job.FetchAlways {
			fileLines = append(fileLines, fmt.Sprintf("  - %s — not downloaded", name))
			continue
		}
		var downloaded, total int64
		anyFailed := false
		lo, hi := m.FileRange(fi)
		for i := lo; i < hi; i++ {
			bytes := int64(m.ArticleBytes(i))
			total += bytes
			switch {
			case p.ArticleFailed(i):
				anyFailed = true
			case p.ArticleDone(i):
				downloaded += bytes
			}
		}
		pct := completionPct(downloaded, total)
		if pct >= 100 && !anyFailed {
			fileLines = append(fileLines, fmt.Sprintf("  ✓ %s — 100%% (%s)",
				name, humanfmt.BytesSI(total)))
			continue
		}
		incomplete++
		fileLines = append(fileLines, fmt.Sprintf("  ⚠ %s — %d%% (%s of %s received)",
			name, pct, humanfmt.BytesSI(downloaded), humanfmt.BytesSI(total)))
	}

	var header string
	if incomplete > 0 {
		header = fmt.Sprintf("File completion (%d of %d incomplete):", incomplete, numFiles)
	} else {
		header = fmt.Sprintf("File completion (%d files, all complete):", numFiles)
	}
	return append([]string{header}, fileLines...)
}

// completionPct returns the integer percentage (0-100) of total bytes that
// downloaded successfully. It floors the result so a file missing any data
// never rounds up to 100, and returns 0 when total is non-positive.
func completionPct(downloaded, total int64) int {
	if total <= 0 {
		return 0
	}
	if downloaded >= total {
		return 100
	}
	return int(float64(downloaded) / float64(total) * 100)
}

// buildFinalFileList creates a file listing of the job's final directory
// for the summary stage. This shows the end state after all processing.
func buildFinalFileList(j *Job) []string {
	dir := j.FinalDir
	if dir == "" {
		dir = j.DownloadDir
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var lines []string
	var totalSize int64
	lines = append(lines, fmt.Sprintf("Final files (%d):", len(entries)))
	for _, e := range entries {
		info, _ := e.Info()
		var sz int64
		if info != nil {
			sz = info.Size()
		}
		if e.IsDir() {
			lines = append(lines, fmt.Sprintf("  📁 %s/", e.Name()))
		} else {
			totalSize += sz
			lines = append(lines, fmt.Sprintf("  %s (%s)", e.Name(), humanfmt.BytesSI(sz)))
		}
	}
	lines = append(lines, fmt.Sprintf("Total: %s", humanfmt.BytesSI(totalSize)))

	return lines
}
