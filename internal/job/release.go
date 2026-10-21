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
// and returns how many of them still have to be fetched. A job already beyond
// repair releases nothing.
//
// A held volume that is complete with no failed article is already on disk, as
// a retried job re-holds the volumes its first run fetched. It is freed
// whatever the sizing, so that FetchedRecoveryBlocks counts it afterwards, and
// it is left out of the cover and of the returned count.
//
// Sized, it frees the cheapest-by-bytes subset of the remaining volumes whose
// names carry a block count that reaches the need, or all of them when they
// cannot reach it, when the need exceeds MaxPar2Slices, or when the cover
// would exceed maxCoverCells. Unsized, it frees every held volume. It reads
// names and sizes from the file table, so residency does not change the result.
func (j *Job) ReleaseRecovery(s RecoverySizing) (released int, err error) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return 0, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	// repairStateLocked, not RepairState: that takes contentMu.RLock, which this holds.
	if j.repairStateLocked().Hopeless() {
		return 0, nil
	}
	var onDisk, held []int
	for _, i := range j.progress.DeferredRecoveryIndices() {
		if f := &j.progress.files[i]; f.Complete && f.FailedBytes == 0 {
			onDisk = append(onDisk, i)
		} else {
			held = append(held, i)
		}
	}
	j.undeferRecovery(onDisk)
	// j.fileTable is non-nil here: the j.progress guard above passed, and
	// AttachFiles is the only assignment of either (content.go), setting both.
	if s.sized {
		var idxs, blocks []int
		var bytes []int64
		for _, i := range held {
			if n, ok := RecoveryVolumeBlocks(j.fileTable.Subject(i)); ok {
				idxs = append(idxs, i)
				blocks = append(blocks, n)
				bytes = append(bytes, j.fileTable.Bytes(i))
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
