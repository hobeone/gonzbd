package job

import "fmt"

// RecoverySizing says how much recovery a release should free. The zero value
// is unsized: release every held volume.
type RecoverySizing struct {
	need  int
	sized bool
}

// NeedBlocks sizes a release to at least n recovery blocks; n < 0 is 0.
func NeedBlocks(n int) RecoverySizing { return RecoverySizing{need: max(n, 0), sized: true} }

// Unsized releases every held volume.
func Unsized() RecoverySizing { return RecoverySizing{} }

// MaxPar2Slices is the PAR2 limit on the input slices of one recovery set, so
// no deficit of a genuine set exceeds it.
const MaxPar2Slices = 32768

// maxCoverCells bounds candidates*(need+1), the cover's table at 4 MiB of bits
// and its DP at 2^25 steps, all under contentMu.
const maxCoverCells = 1 << 25

// ReleaseRecovery frees held recovery volumes (FetchIfNeeded to FetchAlways)
// and returns how many it freed. A job already beyond repair releases nothing.
//
// Sized, it frees the cheapest-by-bytes subset of the volumes whose names
// carry a block count that reaches the need, or every held volume when they
// cannot reach it, when the need exceeds MaxPar2Slices, or when the cover
// would exceed maxCoverCells. Unsized, or without a resident manifest to read
// names and sizes from, it frees every held volume.
func (j *Job) ReleaseRecovery(s RecoverySizing) (released int, err error) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return 0, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	// Computed inline: RepairState takes contentMu.RLock, which this holds.
	if RepairStateFrom(j.progress.ContentFailedBytes(), j.recoveryBytes, j.progress.HasPar2Files()).Hopeless() {
		return 0, nil
	}
	held := j.progress.DeferredRecoveryIndices()
	if s.sized && j.manifest != nil {
		var idxs, blocks []int
		var bytes []int64
		for _, i := range held {
			if n, ok := RecoveryVolumeBlocks(j.manifest.FileSubject(i)); ok {
				idxs = append(idxs, i)
				blocks = append(blocks, n)
				bytes = append(bytes, j.manifest.FileBytes(i))
			}
		}
		if s.need <= MaxPar2Slices && len(idxs)*(s.need+1) <= maxCoverCells {
			if cover, ok := cheapestCover(blocks, bytes, s.need); ok {
				held = held[:0:0]
				for _, k := range cover {
					held = append(held, idxs[k])
				}
			}
		}
	}
	j.undeferRecovery(held)
	return len(held), nil
}
