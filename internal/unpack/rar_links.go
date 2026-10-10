package unpack

import (
	"errors"
	"log/slog"

	"github.com/hobeone/rarengine"
)

// Extraction creates no links in a job. A link member (symlink, junction, hard
// link or file reference) is skipped with a log line and the set carries on:
// its target is chosen by the archive's author, and in DirectUnpack the
// extraction root is the job directory itself, so a hard link could join a
// downloaded volume to another name and a later write through one would
// change the other. A media download has no need for any of them, and
// skipping one costs only that member.

// ExtractedEntryExists reports whether the entry belongs in a result's file
// list: a regular member does, a directory or link member does not.
func ExtractedEntryExists(fh *rarengine.FileHeader) bool {
	return !fh.IsDir && fh.LinkType == rarengine.LinkNone
}

// skipLinkEntry logs a link member and reports it on opts.OnLine. It never
// reads the entry and never touches the extraction root; the caller still
// closes the member, which is where the verdict is reported.
func skipLinkEntry(fh *rarengine.FileHeader, opts Options, log *slog.Logger) {
	log.Warn("go_unrar: skipping link entry: extraction creates no links in a job",
		"name", fh.Name, "link_type", fh.LinkType)
	if opts.OnLine != nil {
		opts.OnLine("Skipping link: " + fh.Name)
	}
}

// NoteDictionaryLimit logs, when err is rarengine's dictionary-window
// refusal, a line that names the limit and what happens next, so it is not
// read as corruption. It is a no-op for any other error. fh may be nil.
func NoteDictionaryLimit(log *slog.Logger, err error, fh *rarengine.FileHeader) {
	if !errors.Is(err, rarengine.ErrDictionaryTooLarge) {
		return
	}
	var name string
	var dict int64
	if fh != nil {
		name, dict = fh.Name, fh.DictSize
	}
	log.Warn("go_unrar: archive needs a larger dictionary window than the pure-Go engine's 32 MiB limit; "+
		"this is a capacity limit, not corruption. The external unrar is tried next only when "+
		"go_rar_fallback is on and unrar is installed; otherwise the set fails",
		"member", name, "declared_dict_bytes", dict)
}
