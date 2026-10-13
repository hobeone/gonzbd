package unpack

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"syscall"

	"github.com/bodgit/sevenzip"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/humanfmt"
	"github.com/hobeone/gonzbd/internal/rarheader"
)

// ErrInsufficientSpace is returned, with FailDiskFull, for an archive whose
// declared unpacked size does not fit in the free space above
// Options.MinFreeSpace.
var ErrInsufficientSpace = errors.New("unpack: not enough free space to extract")

// errSizeUnknown is returned by declaredSize for an archive whose headers do
// not state what it unpacks to.
var errSizeUnknown = errors.New("unpack: declared size unknown")

// checkFreeSpace refuses an extraction of archive into outDir that would
// leave less than minFree bytes free: it compares what the archive's headers
// declare it unpacks to with the free space on outDir's filesystem. It is the
// bound on how much an extraction may write, applied the same way whichever
// engine runs; DirectUnpack applies it through CheckRarVolumesFreeSpace, and
// both make the comparison in refuseUnlessItFits.
//
// It refuses nothing it cannot measure. An archive whose headers do not state
// their size (encrypted RAR headers, an unreadable header) and a free-space
// query that fails are logged and let through, and the extraction then fails
// as FailDiskFull if it fills the disk (withDiskFull). So does an archive
// whose headers understate its contents, but rarengine caps each RAR member
// at its declared size.
func checkFreeSpace(ctx context.Context, log *slog.Logger, archive Archive, outDir, password string, minFree int64) error {
	declared, err := declaredSize(archive, password)
	return refuseUnlessItFits(ctx, log, archive.Name, archive.MainFile, declared, err, outDir, minFree)
}

// CheckRarVolumesFreeSpace is checkFreeSpace for a RAR set of which only
// volumes, in playback order from the first, are counted: DirectUnpack's
// bound, applied while later volumes are still downloading. It counts each
// member whose header starts in one of volumes at the full unpacked size that
// header declares, so a member continuing into later volumes is counted
// whole, and a member starting in a later volume is not counted at all.
func CheckRarVolumesFreeSpace(ctx context.Context, log *slog.Logger, name string, volumes []string, outDir string, minFree int64) error {
	if len(volumes) == 0 {
		return nil
	}
	declared, err := rarDeclaredSize(volumes)
	return refuseUnlessItFits(ctx, log, name, volumes[0], declared, err, outDir, minFree)
}

// refuseUnlessItFits is checkFreeSpace's comparison, for an archive whose
// headers declared declared bytes, or sizeErr when they could not be read.
func refuseUnlessItFits(ctx context.Context, log *slog.Logger, name, mainFile string, declared int64, sizeErr error, outDir string, minFree int64) error {
	if sizeErr != nil {
		log.Info("unpack: no free-space check, the archive's unpacked size is not known",
			"archive", mainFile, "err", sizeErr)
		return nil
	}
	if declared == 0 {
		return nil
	}
	free, err := freeBytes(ctx, outDir)
	if err != nil {
		log.Warn("unpack: no free-space check, free space unreadable",
			"dir", outDir, "err", err)
		return nil
	}
	if !fits(declared, free, minFree) {
		return fmt.Errorf("%w: %s unpacks to %s, and %s has %s free with %s (min_free_space) to keep",
			ErrInsufficientSpace, name, humanfmt.Bytes(declared), outDir,
			humanfmt.Bytes(free), humanfmt.Bytes(minFree))
	}
	return nil
}

// fits reports whether declared bytes can be written to a filesystem with
// free bytes free and still leave minFree free.
func fits(declared, free, minFree int64) bool {
	return declared <= free-minFree
}

// freeBytes returns the bytes free on dir's filesystem.
func freeBytes(ctx context.Context, dir string) (int64, error) {
	// assembler.FreeBytes needs a context it can abandon a stuck statfs on.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	return assembler.FreeBytes(ctx, dir)
}

// declaredSize returns the total unpacked size archive's headers declare,
// or errSizeUnknown when they do not declare one. It reads headers only.
func declaredSize(archive Archive, password string) (int64, error) {
	switch archive.Type {
	case RarArchive:
		vols, err := volumesForArchive(archive)
		if err != nil {
			return 0, err
		}
		return rarDeclaredSize(vols)
	case SevenZipArchive:
		return sevenZipDeclaredSize(archive.MainFile, password)
	case TarArchive:
		return tarDeclaredSize(archive.MainFile)
	case SplitArchive:
		var n int64
		for _, p := range archive.Parts {
			st, err := os.Stat(p)
			if err != nil {
				return 0, err
			}
			n += st.Size()
		}
		return n, nil
	default:
		return 0, errSizeUnknown
	}
}

// rarDeclaredSize is declaredSize for the RAR volumes vols, given in playback
// order.
func rarDeclaredSize(vols []string) (int64, error) {
	n, err := rarheader.UnpackedSize(vols)
	if errors.Is(err, rarheader.ErrSizeUnknown) {
		return 0, errSizeUnknown
	}
	return n, err
}

func sevenZipDeclaredSize(mainFile, password string) (int64, error) {
	var r *sevenzip.ReadCloser
	var err error
	if password != "" {
		r, err = sevenzip.OpenReaderWithPassword(mainFile, password)
	} else {
		r, err = sevenzip.OpenReader(mainFile)
	}
	if err != nil {
		return 0, err
	}
	defer r.Close() //nolint:errcheck // read-only
	var n int64
	for _, f := range r.File {
		n += int64(f.UncompressedSize) //nolint:gosec // a size the archive declares; a wrapped value is a corrupt header, which the extraction then reports
	}
	return n, nil
}

// tarDeclaredSize sums the regular-file entries' sizes. tar.Reader seeks
// over each entry's data in an *os.File rather than reading it.
func tarDeclaredSize(mainFile string) (n int64, err error) {
	f, err := os.Open(mainFile) //nolint:gosec // read-only open of an archive path resolved by unpack.Scan
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return 0, err
		}
		if hdr.Typeflag == tar.TypeReg {
			n += hdr.Size
		}
	}
}

// withDiskFull classifies an extraction that failed because the disk filled
// as FailDiskFull, whatever the engine made of the error, and so does a
// refusal by checkFreeSpace. extractInStage applies it to every archive
// extractor's attempt, and FileJoin to its own result. The external tools'
// exit statuses carry no errno; ClassifyUnrarOutput and
// Classify7zOutput read their disk-full messages instead.
func withDiskFull(res Result, err error) (Result, error) {
	if IsDiskFull(err) {
		res.Reason = FailDiskFull
	}
	return res, err
}

// IsDiskFull reports whether err is an extraction failure that is classified
// FailDiskFull: the disk filled (ENOSPC), or a free-space check refused the
// extraction (ErrInsufficientSpace).
func IsDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, ErrInsufficientSpace)
}
