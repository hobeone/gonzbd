package job

import "fmt"

// Activity is what is executing right now. It is NOT a state: nothing
// branches on it, no transition consults it, and it is written by whichever
// component is doing the work. It exists for the UI, the API and the log.
//
// This is where the old model's post-processing stages went. They were states
// there, which is why its transition table needed a near-complete subgraph
// over them — the model had no way to say "still producing, doing something
// else now" (spec §1.1).
type Activity uint8

const (
	// ActNone means no work is executing. The zero value, so a StateView for
	// a job that has never run reports it without anyone assigning it.
	ActNone Activity = iota
	// ActCRCCheck is the cheap verification path — file CRCs against par2
	// headers, no par2 process. Runs inside Assessing.
	ActCRCCheck
	// ActPar2Verify is the full verification path. Runs inside Assessing.
	ActPar2Verify
	// ActPar2Repair runs inside Repairing.
	ActPar2Repair
	// ActVolumeRecovery renames obfuscated RAR volumes so sets are
	// detectable. Runs inside Extracting.
	ActVolumeRecovery
	// ActUnpack runs inside Extracting.
	ActUnpack
	// ActDeobfuscate restores clean filenames. Runs inside Finalizing.
	ActDeobfuscate
	// ActCleanup removes samples, par2 files, unwanted extensions and
	// sidecar directories. Runs inside Finalizing.
	ActCleanup
	// ActMove relocates files to the final directory. Runs inside Finalizing.
	ActMove
	// ActScript runs the user post-processing script, inside Finalizing.
	ActScript
)

// AllActivities returns every declared activity.
func AllActivities() []Activity {
	return []Activity{
		ActNone, ActCRCCheck, ActPar2Verify, ActPar2Repair, ActVolumeRecovery,
		ActUnpack, ActDeobfuscate, ActCleanup, ActMove, ActScript,
	}
}

func (a Activity) String() string {
	switch a {
	case ActNone:
		return "None"
	case ActCRCCheck:
		return "CRCCheck"
	case ActPar2Verify:
		return "Par2Verify"
	case ActPar2Repair:
		return "Par2Repair"
	case ActVolumeRecovery:
		return "VolumeRecovery"
	case ActUnpack:
		return "Unpack"
	case ActDeobfuscate:
		return "Deobfuscate"
	case ActCleanup:
		return "Cleanup"
	case ActMove:
		return "Move"
	case ActScript:
		return "Script"
	default:
		return fmt.Sprintf("Activity(%d)", uint8(a))
	}
}
