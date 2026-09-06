package app

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// BuildIngestJob converts an already-parsed NZB plus caller-supplied options
// into a runtime Job and Header, resolving sanitize/category/on-demand-par2
// settings from live config. It is a free function rather than an *Application
// method so that internal/api (which holds its own *config.Config but no
// *Application reference) can call it without new interface plumbing.
//
// It does not parse the NZB (parse failures and job-construction failures
// need to stay distinguishable at call sites, e.g. HTTP 400 vs 500) and does
// not enqueue the job via Application.AddJob (callers may need to inspect
// the constructed job, e.g. reject an empty manifest, before enqueueing).
func BuildIngestJob(cfg *config.Config, parsed *nzb.NZB, filename string, opts types.FetchOptions, logger *slog.Logger) (*job.Job, dispatch.Header, error) {
	id := opts.JobID
	if id == "" {
		var err error
		id, err = newJobID()
		if err != nil {
			return nil, dispatch.Header{}, err
		}
	}

	name := opts.NzbName
	if name == "" {
		name = deriveName(filename)
	} else {
		// Strip .nzb extension when the caller supplies an explicit name
		// (e.g. Sonarr's nzbname parameter often includes it).
		name = stripNZBExt(name)
	}

	var sOpts fsutil.SanitizeOptions
	var categories []config.CategoryConfig
	var onDemandPar2 bool
	if cfg != nil {
		snap := cfg.IngestSnapshot()
		sOpts = snap.Downloads.SanitizeOptions()
		categories = snap.Categories
		onDemandPar2 = snap.Downloads.OnDemandPar2
	}

	// 1. Apply regex-based cleanup (strip spam prefixes/suffixes)
	name = fsutil.CleanupName(name, sOpts)

	// 2. Apply filesystem sanitization rules.
	name = fsutil.SanitizeFolderName(name, sOpts)

	// Resolve sentinel values from the matching category config.
	pp := opts.PP
	script := opts.Script
	priority := opts.Priority
	ppReason := "explicit"
	cat := config.FindCategory(categories, opts.Category)
	if pp == types.PPInherit {
		pp = cat.PP
		ppReason = fmt.Sprintf("category %q", cat.Name)
	}
	if script == "" {
		script = cat.Script
	}
	if pp > types.PPDelete {
		pp = types.PPDelete
	} else if pp < types.PPNone {
		pp = types.PPNone
	}
	if priority == constants.DefaultPriority {
		p := cat.Priority
		if p < -128 || p > 127 {
			priority = constants.NormalPriority
		} else {
			priority = constants.Priority(p)
		}
	}

	if logger != nil {
		logger.Info("job PP resolved",
			"job_name", name,
			"category", opts.Category,
			"requested_pp", opts.PP,
			"resolved_pp", pp,
			"reason", ppReason,
			"script", script,
			"priority", priority.String(),
		)
	}

	logParseAnomalies(logger, filename, parsed)

	files := make([]job.JobFile, 0, len(parsed.Files))
	for _, pf := range parsed.Files {
		isPar2 := job.IsPar2File(pf.Subject)
		isRecovery := isPar2 && job.IsRecoveryVolume(pf.Subject)
		jf := job.JobFile{
			Subject:        pf.Subject,
			Date:           pf.Date,
			Bytes:          pf.Bytes,
			Articles:       make([]job.JobArticle, 0, len(pf.Articles)),
			IsPar2Recovery: isRecovery,
			Deferred:       isRecovery && onDemandPar2,
		}
		for _, pa := range pf.Articles {
			jf.Articles = append(jf.Articles, job.JobArticle{
				ID:     pa.ID,
				Bytes:  pa.Bytes,
				Number: pa.Number,
			})
		}
		files = append(files, jf)
	}
	job.SortJobFiles(files)

	manifest := job.NewManifest(files)
	pol := job.Policy{
		Repair: pp >= types.PPRepair,
		Unpack: pp >= types.PPUnpack,
		Delete: pp >= types.PPDelete,
	}
	j := job.New(id, name, pol)
	if err := j.AttachContent(manifest); err != nil {
		return nil, dispatch.Header{}, fmt.Errorf("create job %q: %w", filename, err)
	}
	for fi, jf := range files {
		if jf.Deferred {
			_ = j.SetFileFetchPolicy(fi, job.FetchIfNeeded)
		}
	}
	if priority == constants.PausedPriority {
		_ = j.SetIntent(job.IntentPause)
	}

	hdr := dispatch.Header{
		Name:     name,
		Filename: filename,
		Category: opts.Category,
		Priority: int(priority),
		Bytes:    manifest.TotalBytes(),
		Warning:  parseAnomalySummary(parsed),
		Script:   script,
		Password: opts.Password,
		PP:       pp,
		URL:      "",
		MD5:      hex.EncodeToString(parsed.MD5[:]),
	}

	return j, hdr, nil
}

// stripNZBExt removes .nzb, .nzb.gz, and .nzb.bz2 extensions from name.
// Used for both explicit names and derived names to prevent download
// directories like "movie.nzb/".
func stripNZBExt(name string) string {
	lower := strings.ToLower(name)
	for _, suffix := range []string{".nzb.gz", ".nzb.bz2", ".nzb"} {
		if strings.HasSuffix(lower, suffix) {
			return name[:len(name)-len(suffix)]
		}
	}
	return name
}

// deriveName strips directory components and the extension from path.
// For "/watch/My.Show.S01E02.nzb" returns "My.Show.S01E02". A ".nzb.gz"
// or ".nzb.bz2" double extension is collapsed to the bare stem too.
func deriveName(path string) string {
	base := filepath.Base(path)
	if stripped := stripNZBExt(base); stripped != base {
		return stripped
	}
	if ext := filepath.Ext(base); ext != "" {
		return base[:len(base)-len(ext)]
	}
	return base
}

// newJobID returns a 16-character lowercase hex string backed by 8
// bytes (64 bits) of OS entropy.
func newJobID() (string, error) { //nocover: OS entropy error branch
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// parseAnomalySummary renders the parser's discard counters as one short
// user-facing line, or "" when the document was clean.
//
// Kept separate from logParseAnomalies because the two have different
// audiences and different length budgets: the log line can afford one
// key/value pair per counter, this one is displayed in a queue row.
//
// Only counters whose segments were DISCARDED belong here. The parser also
// records anomalies on segments it KEPT — a Message-ID that violates the RFC
// but still names a fetchable article — and folding those into this sentence
// would tell the user data was dropped when none was. Those go to
// logParseAnomalies alone, where the reader is an operator gathering evidence
// rather than someone deciding whether to re-add a job.
func parseAnomalySummary(parsed *nzb.NZB) string {
	if parsed == nil {
		return ""
	}
	var parts []string
	if n := parsed.DuplicateMessageIDs; n > 0 {
		parts = append(parts, fmt.Sprintf("%d repeated message-id", n))
	}
	if n := parsed.EmptyMessageIDs; n > 0 {
		parts = append(parts, fmt.Sprintf("%d empty message-id", n))
	}
	if n := parsed.OversizeMessageIDs; n > 0 {
		parts = append(parts, fmt.Sprintf("%d over-long message-id", n))
	}
	if n := parsed.MalformedMessageIDs; n > 0 {
		parts = append(parts, fmt.Sprintf("%d unusable message-id", n))
	}
	if n := parsed.DuplicateArticles; n > 0 {
		parts = append(parts, fmt.Sprintf("%d duplicate part number", n))
	}
	if n := parsed.BadArticles; n > 0 {
		parts = append(parts, fmt.Sprintf("%d implausible size", n))
	}
	if n := parsed.SkippedFiles; n > 0 {
		parts = append(parts, fmt.Sprintf("%d file with no usable segments", n))
	}
	if len(parts) == 0 {
		return ""
	}
	return "NZB had malformed segments discarded at ingest: " + strings.Join(parts, ", ")
}

// logParseAnomalies reports what the parser discarded, once per ingest.
//
// The nzb package counts these and returns them; until now nothing read them,
// so an NZB could lose segments to a size check or a repeated Message-ID and
// say so to no one. The job then downloads short and the user learns about it
// from par2, or from a file that never completes.
//
// This is the only place all four nzb.Parse call sites converge, and the only
// one holding both the counters and a logger — the parser deliberately has
// neither a logger nor any knowledge of who called it.
//
// A repeated Message-ID is called out separately from the other counters
// because it is the one whose absence downstream code is entitled to assume:
// dropping it is what makes Message-ID a unique key within a job.
func logParseAnomalies(logger *slog.Logger, filename string, parsed *nzb.NZB) {
	if logger == nil || parsed == nil {
		return
	}

	discarded := parsed.DuplicateMessageIDs + parsed.DuplicateArticles +
		parsed.BadArticles + parsed.SkippedFiles + parsed.EmptyMessageIDs +
		parsed.OversizeMessageIDs + parsed.MalformedMessageIDs
	kept := parsed.NonConformantMessageIDs + parsed.NonASCIIMessageIDs +
		parsed.MessageIDsMissingAtSign

	if discarded > 0 {
		logger.Warn("NZB contains malformed segments; they were discarded at ingest",
			"filename", filename,
			"duplicate_message_ids", parsed.DuplicateMessageIDs,
			"duplicate_part_numbers", parsed.DuplicateArticles,
			"implausible_size", parsed.BadArticles,
			"empty_message_ids", parsed.EmptyMessageIDs,
			"oversize_message_ids", parsed.OversizeMessageIDs,
			"malformed_message_ids", parsed.MalformedMessageIDs,
			"files_without_usable_segments", parsed.SkippedFiles,
		)
	}

	// Logged separately, and at Info, because nothing was lost: these
	// segments download normally. They are recorded so that a decision to
	// promote any of these rules to a rejection can rest on how often the
	// shape actually occurs rather than on assumption.
	if kept > 0 {
		logger.Info("NZB contains non-conformant message-ids; they were kept and will be fetched",
			"filename", filename,
			"over_rfc_length", parsed.NonConformantMessageIDs,
			"non_printable_ascii", parsed.NonASCIIMessageIDs,
			"missing_at_sign", parsed.MessageIDsMissingAtSign,
		)
	}
}
