package queue

// RepairState is the verdict on whether a job's damaged content can be
// rebuilt from its par2 recovery capacity.
//
// It exists because three call sites reached this verdict independently, from
// raw figures, and drifted. Each fix to the comparison had to be applied three
// times, and the third site — the queue listing, which re-derives the
// comparison in TypeScript — was invisible to a reference search on the Go
// accessors and was missed twice running. See #318.
//
// The verdict is a proxy and always has been. Par2 repair is decided by block
// counts, not bytes; bytes are the only signal available before the volumes
// are fetched and parsed. Centralizing it does not make it correct, it makes
// it correctable in one place — see #334 for replacing it outright.
//
// One hopeless verdict is deliberately outside this type: failMsgForJob also
// aborts when every byte a job set out to fetch has failed, which it decides
// on total rather than content bytes, against ExpectedBytes. A job of nothing
// but par2 files, all of which fail, therefore reports RepairIntact — its
// content damage really is zero — while being finalized as beyond repair. The
// figures this type derives from cannot express that case, and widening it to
// take ExpectedBytes would put a fourth quantity into the dispatch gate for a
// degenerate shape. The consequence is confined to the queue row's label.
type RepairState string

const (
	// RepairIntact means no content is damaged. Par2 files may have failed;
	// that costs capacity, not content.
	RepairIntact RepairState = "intact"

	// RepairPossible means the damage is within the recognized capacity. This
	// is the *unsound* direction of the comparison: scattered damage destroys
	// more blocks than its byte count suggests, so this is "not proven
	// hopeless", never "proven repairable".
	RepairPossible RepairState = "repairable"

	// RepairUnknown means the job carries par2 files but none were recognized
	// as recovery volumes, so its capacity is unmeasured rather than absent.
	// The PAR2 specification recommends the .volNNN+MM name but does not
	// require it and does not forbid recovery slices in a plainly-named
	// .par2; par2 itself reads packets and ignores names. Condemning a job on
	// this discards a download that par2 may well repair.
	RepairUnknown RepairState = "unknown"

	// RepairNoCapacity means content is damaged and the job carries no par2
	// file of any kind. Unlike RepairUnknown, zero here is a finding.
	RepairNoCapacity RepairState = "no_capacity"

	// RepairBeyondCapacity means damaged content exceeds what the recognized
	// recovery volumes could rebuild. This is the sound direction: capacity
	// may be understated, so this errs toward aborting.
	RepairBeyondCapacity RepairState = "beyond_capacity"
)

// AllRepairStates lists every declared state, so that code which must handle
// all of them can be tested against the list rather than against whichever
// states its author happened to remember.
//
// The wire values are part of the contract: ui/src/lib/types.ts declares the
// same set as a union, and a client that receives an unlisted state falls back
// to a neutral label. Changing one of these strings without changing that
// union silently degrades the queue row. See repair_state_exhaustive_test.go,
// which pins both the list and the strings. Same construction as
// constants.AllStatuses (#291), postproc.AllQuickCheckOutcomes (#313) and
// AllFetchPolicies (#331).
func AllRepairStates() []RepairState {
	return []RepairState{
		RepairIntact,
		RepairPossible,
		RepairUnknown,
		RepairNoCapacity,
		RepairBeyondCapacity,
	}
}

// Hopeless reports whether the state is a definite verdict that the job cannot
// be repaired, and is therefore grounds to stop work on it.
//
// The two states it excludes are excluded for different reasons, and both
// matter: RepairPossible is the unsound direction of the comparison, and
// RepairUnknown is ignorance rather than a finding. Neither is a basis for
// discarding a download.
func (s RepairState) Hopeless() bool {
	return s == RepairNoCapacity || s == RepairBeyondCapacity
}

// RepairStateFrom derives the verdict from the three quantities it depends on.
//
// Exported in this form, rather than only as a method, because the dispatch
// snapshot already holds these figures under the queue lock and reads them
// from the manifest directly. Routing it back through Job's accessors there
// would take a second mutex inside the first for values it already has.
//
// contentFailedBytes must exclude par2 files (see JobProgress.ContentFailedBytes):
// a failed par2 file is lost capacity, not damage needing repair. Passing the
// total instead condemns a job for losing the file whose only purpose was to
// rescue others.
func RepairStateFrom(contentFailedBytes, recoveryBytes int64, hasPar2Files bool) RepairState {
	if contentFailedBytes == 0 {
		return RepairIntact
	}
	if recoveryBytes == 0 {
		if hasPar2Files {
			return RepairUnknown
		}
		return RepairNoCapacity
	}
	if contentFailedBytes > recoveryBytes {
		return RepairBeyondCapacity
	}
	return RepairPossible
}

// RepairState reports whether this job's damaged content is within its par2
// recovery capacity.
//
// Safe at any residency: every input derives from the promoted scalars or from
// JobProgress, both of which outlive manifest eviction. That is load-bearing —
// one caller runs over a queue listing, which includes jobs whose manifests
// have been evicted, and a figure that changed with residency would condemn or
// spare the same job according to whether it happened to be promoted.
func (j *Job) RepairState() RepairState {
	p := j.Progress()
	return RepairStateFrom(p.ContentFailedBytes(), j.RecoveryBytes(), p.HasPar2Files())
}
