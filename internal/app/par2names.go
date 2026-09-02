package app

import (
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/queue"
)

// resolvedName is the name a file currently has on disk, as the rest of the
// system understands it: the manifest's subject unless a resolved filename has
// been recorded, which is what every par2 call site already prefers.
//
// It stays true for the whole download because nothing on this path moves
// files any more. A rename during download would break it — the field cannot
// hold a path (fsutil.SanitizeFilename rewrites "/" to "_"), so a file
// relocated into a subdirectory could not be recorded truthfully, and the
// startup resume sweep would stat a top-level path that does not exist.
// Relocation belongs to post-processing, where nothing re-derives a verdict
// from these names afterwards.
func resolvedName(m *queue.Manifest, p *queue.JobProgress, fi int) string {
	name := m.FileSubject(fi)
	if fn := p.FileFilename(fi); fn != "" {
		name = fn
	}
	return name
}

// assembledFiles is what only the queue can tell par2: the name each delivered
// file currently has on disk, and the CRC32 computed for it during download.
//
// The CRCs come from the durability record. The assembler used to combine the
// per-article CRCs it happened to see, which was #349 — a resumed run never
// receives the articles an earlier run completed, so its parts do not tile the
// file — and that writer is gone. A durable run combines the CRCs of the
// articles that abut as they join it, across restarts, so when a file collapses
// to one row that row's crc32 IS the whole-file CRC; Application.recordAssembledCRC
// copies it onto the queue when the file finalizes. A file that keeps more than
// one row reads as CRC 0, which is R23's "unavailable" rather than a CRC of
// zero, and par2Verdict treats it conservatively.
func assembledFiles(m *queue.Manifest, p *queue.JobProgress) []par2.AssembledFile {
	files := make([]par2.AssembledFile, m.NumFiles())
	for fi := range m.NumFiles() {
		files[fi] = par2.AssembledFile{
			FileName: resolvedName(m, p, fi),
			CRC32:    p.FileAssembledCRC32(fi),
		}
	}
	return files
}
