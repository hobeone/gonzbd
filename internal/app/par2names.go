package app

import (
	"github.com/hobeone/gonzbd/internal/par2"
)

// resolvedName is the name a file currently has on disk, as the rest of the
// system understands it: the manifest's subject unless a resolved filename has
// been recorded, which is what every par2 call site already prefers.
//
// It stays true for the whole download because nothing on this path moves
// files any more. A rename during download would break it — the field cannot
// hold a path (fsutil.SanitizeFilename rewrites "/" to "_"), so a file
// relocated into a subdirectory could not be recorded truthfully, and a
// restart's verification would read a top-level path that does not exist.
// Relocation belongs to post-processing, where nothing re-derives a verdict
// from these names afterwards.
type manifestReader interface {
	FileSubject(fileIdx int) string
	FileBytes(fileIdx int) int64
	NumFiles() int
}

type progressReader interface {
	FileFilename(fileIdx int) string
	FileAssembledCRC32(fileIdx int) uint32
}

func resolvedName(m manifestReader, p progressReader, fi int) string {
	name := m.FileSubject(fi)
	if fn := p.FileFilename(fi); fn != "" {
		name = fn
	}
	return name
}

// assembledFiles is what only the queue can tell par2: the name each delivered
// file currently has on disk, and the CRC32 computed for it during download.
//
// The CRCs come from the article record: when a file completes,
// Job.SettleFileCRC combines its written rows' CRCs and stores the result, so a
// resumed run, which never receives the articles an earlier run wrote, still
// gets the whole-file CRC from their verified rows (#349). A file whose rows do
// not tile it gapless from offset 0, or that has a failed article, reads as
// CRC 0, which is R23's "unavailable" rather than a CRC of zero, and
// par2Verdict treats it conservatively.
func assembledFiles(m manifestReader, p progressReader) []par2.AssembledFile {
	files := make([]par2.AssembledFile, m.NumFiles())
	for fi := range m.NumFiles() {
		files[fi] = par2.AssembledFile{
			FileName: resolvedName(m, p, fi),
			CRC32:    p.FileAssembledCRC32(fi),
		}
	}
	return files
}
