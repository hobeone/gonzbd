package par2

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RepairPhase represents the current phase of a par2 repair operation.
type RepairPhase int

const (
	// PhaseVerifying is the initial verification phase.
	PhaseVerifying RepairPhase = iota
	// PhaseExtraFiles is when par2 scans extra files for data blocks.
	PhaseExtraFiles
	// PhaseRepairing is when par2 is actively repairing files.
	PhaseRepairing
	// PhaseVerifyingRepair is when par2 verifies the repaired files.
	PhaseVerifyingRepair
)

// RepairOutput captures all parsed state from par2's stdout/stderr.
// This replaces the bulk strings.Contains approach with line-by-line
// parsing that tracks phase transitions, progress, renames, and
// used_for_repair/used_joinables — matching SABnzbd's par2cmdline_verify.
type RepairOutput struct {
	// Phase is the current par2 processing phase.
	Phase RepairPhase

	// Finished is true when par2 reported all files correct or repair complete.
	Finished bool

	// NeedMoreBlocks is true when par2 reported "You need N more recovery blocks".
	NeedMoreBlocks bool
	// BlocksNeeded is the number of additional recovery blocks required.
	BlocksNeeded int

	// Renames maps par2's canonical filename → the actual on-disk filename.
	// Populated from "is a match for" lines. SABnzbd uses this to track
	// which files par2 will rename during repair.
	Renames map[string]string

	// UsedForRepair lists files that par2 used as repair sources
	// (detected via "duplicate data blocks" lines). These files should
	// not be deleted during cleanup.
	UsedForRepair []string

	// UsedJoinables lists joinable files (.001/.002) that par2 consumed
	// for reconstruction (detected via "data blocks from" lines).
	UsedJoinables []string

	// RecoverableFiles is the total number of recoverable files in the set,
	// parsed from "There are N recoverable files".
	RecoverableFiles int

	// VerifyTotal is the count of files to verify (missing + damaged).
	VerifyTotal int

	// VerifyNum is the current verification progress counter.
	VerifyNum int

	// RepairPercent is the last reported repair progress percentage.
	RepairPercent float64

	// RepairStarted is the time the repair phase began. Set when
	// "Repair is possible" is parsed (phase transition to PhaseRepairing).
	// Used by ETA() to estimate remaining repair time.
	RepairStarted time.Time

	// Status is the parsed final status from the output.
	Status Status

	// InvalidOption is true when par2 rejected a command-line option.
	InvalidOption bool

	// Lines holds all non-progress output lines (matching SABnzbd's
	// filter that excludes "Scanning:", "Loading:", "Solving:",
	// "Constructing:", "Repairing:").
	Lines []string
}

// ETA returns the estimated time remaining for the repair based on the
// current RepairPercent and elapsed time since RepairStarted. Returns
// zero if the repair hasn't started or percent is 0. Matches SABnzbd's
// ETA calculation in par2cmdline_verify.
func (ro *RepairOutput) ETA() time.Duration {
	if ro.RepairStarted.IsZero() || ro.RepairPercent <= 0 || ro.RepairPercent >= 100 {
		return 0
	}
	elapsed := time.Since(ro.RepairStarted)
	// ETA = elapsed * (remaining% / done%)
	remaining := (100 - ro.RepairPercent) / ro.RepairPercent
	return time.Duration(float64(elapsed) * remaining)
}

// Compiled regexes for par2 output line parsing.
// These match the exact patterns emitted by par2cmdline and par2cmdline-turbo.
var (
	// isMatchForRE captures "File: "X" - is a match for "Y"".
	// SABnzbd: PAR2_IS_MATCH_FOR_RE.
	isMatchForRE = regexp.MustCompile(`File: "([^"]+)" - is a match for "([^"]+)"`)

	// blockFoundRE captures "File: "X" - found N of M data blocks from "Y"".
	// SABnzbd: PAR2_BLOCK_FOUND_RE.
	blockFoundRE = regexp.MustCompile(`File: "([^"]+)" - found \d+ of \d+ data blocks from "([^"]+)"`)

	// targetRE captures file verification lines: 'File: "X" -' or 'Target: "X" -'.
	// SABnzbd: PAR2_TARGET_RE.
	targetRE = regexp.MustCompile(`^(?:File|Target): "(.+)" -`)

	// recoverableFilesRE extracts the file count from
	// "There are N recoverable files".
	recoverableFilesRE = regexp.MustCompile(`There are (\d+) recoverable files`)

	// repairProgressRE extracts the percentage from "Repairing: NN.N%" or
	// "Processing: NN.N%". The "Processing:" variant appears when par2 is
	// only joining files without actual repair.
	repairProgressRE = regexp.MustCompile(`^(?:Repairing|Processing):\s+(\d+(?:\.\d+)?)%`)

	// missingDamagedRE extracts file counts from "N file(s) are missing."
	// or "N file(s) exist but are damaged."
	missingDamagedRE = regexp.MustCompile(`^(\d+) file`)

	// progressFilterPrefixes are line prefixes that are filtered from the
	// retained Lines slice (they are high-frequency progress updates).
	progressFilterPrefixes = []string{
		"Repairing:",
		"Scanning:",
		"Loading:",
		"Solving:",
		"Constructing:",
	}
)

// ParseRepairOutput processes par2's combined stdout/stderr line by line,
// tracking phase transitions and extracting structured data. This mirrors
// SABnzbd's par2cmdline_verify state machine from newsunpack.py.
func ParseRepairOutput(output string) *RepairOutput {
	ro := &RepairOutput{
		Phase:   PhaseVerifying,
		Renames: make(map[string]string),
	}

	inVerify := false
	inExtraFiles := false
	verified := false // true once "Repair is required" is seen

	for rawLine := range strings.SplitSeq(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		// Filter progress lines from retained output (SABnzbd does this too).
		isProgress := false
		for _, prefix := range progressFilterPrefixes {
			if strings.HasPrefix(line, prefix) {
				isProgress = true
				break
			}
		}
		if !isProgress {
			ro.Lines = append(ro.Lines, line)
		}

		// --- Error/termination patterns (checked first, highest priority) ---

		if strings.HasPrefix(line, "Invalid option specified") ||
			strings.HasPrefix(line, "Invalid thread option") ||
			strings.HasPrefix(line, "Cannot specify recovery file count") {
			ro.InvalidOption = true
			ro.Status = StatusBadOption
			continue
		}

		if strings.HasPrefix(line, "All files are correct") {
			ro.Finished = true
			ro.Status = StatusAllFilesOK
			continue
		}

		if strings.HasPrefix(line, "Repair is required") {
			ro.Status = StatusRepairRequired
			verified = true
			// Reset counters for verification of repair.
			ro.VerifyTotal = 0
			ro.VerifyNum = 0
			continue
		}

		if strings.HasPrefix(line, "Main packet not found") ||
			strings.Contains(line, "The recovery file does not exist") {
			ro.Status = StatusInvalidPar2
			continue
		}

		if strings.HasPrefix(line, "You need") {
			if m := needMoreBlocksRE.FindStringSubmatch(line); len(m) == 2 {
				ro.NeedMoreBlocks = true
				ro.BlocksNeeded, _ = strconv.Atoi(m[1])
				ro.Status = StatusNeedMoreBlocks
			}
			continue
		}

		if strings.HasPrefix(line, "Repair is possible") {
			ro.Status = StatusRepairPossible
			ro.Phase = PhaseRepairing
			ro.RepairPercent = 0
			ro.RepairStarted = time.Now()
			continue
		}

		if strings.HasPrefix(line, "Repair is not possible") {
			ro.Status = StatusRepairNotPossible
			continue
		}

		if m := repairProgressRE.FindStringSubmatch(line); len(m) == 2 {
			if pct, err := strconv.ParseFloat(m[1], 64); err == nil {
				ro.RepairPercent = pct
			}
			continue
		}

		if strings.HasPrefix(line, "Repair complete") {
			ro.Finished = true
			// Don't override a more-specific failure status.
			if ro.Status != StatusRepairFailed && ro.Status != StatusRepairNotPossible {
				ro.Status = StatusRepairComplete
			}
			continue
		}

		// Count missing/damaged files after "Repair is required".
		if verified && (strings.HasSuffix(line, "are missing.") || strings.HasSuffix(line, "exist but are damaged.")) {
			if m := missingDamagedRE.FindStringSubmatch(line); len(m) == 2 {
				n, _ := strconv.Atoi(m[1])
				ro.VerifyTotal += n
			}
			continue
		}

		if strings.HasPrefix(line, "Verifying repaired files") {
			ro.Phase = PhaseVerifyingRepair
			continue
		}

		if ro.Phase == PhaseVerifyingRepair && strings.HasPrefix(line, "Target") {
			ro.VerifyNum++
			continue
		}

		if strings.Contains(line, "Could not write") && strings.Contains(line, "at offset 0:") {
			// Join-output collision — par2 tried to write but the file exists.
			// SABnzbd treats this as finished=true when joinables exist.
			// We flag it; the caller decides based on joinable state.
			ro.Finished = true
			continue
		}

		if strings.Contains(line, "cannot be renamed to") {
			ro.Status = StatusRepairNotPossible
			continue
		}

		if strings.Contains(line, "not enough space on the disk") ||
			strings.Contains(line, "Insufficient disk space") {
			ro.Status = StatusDiskFull
			continue
		}

		if strings.HasPrefix(line, "No details available for recoverable file") {
			ro.Status = StatusRepairNotPossible
			continue
		}

		if strings.HasPrefix(line, "Repair Failed") {
			ro.Status = StatusRepairFailed
			ro.Finished = false
			continue
		}

		// --- Phase-dependent parsing (only before "Repair is required") ---

		if !verified {
			// "Scanning:" lines are silently consumed (SABnzbd: pass).
			if strings.HasPrefix(line, "Scanning:") {
				continue
			}

			if inExtraFiles {
				// Rename detection: "File: "X" - is a match for "Y""
				if m := isMatchForRE.FindStringSubmatch(line); len(m) == 3 {
					oldName := m[1]
					newName := m[2]
					ro.Renames[newName] = oldName
					ro.VerifyNum++
				}

				// Block reconstruction: "File: "X" - found N of M data blocks from "Y""
				if m := blockFoundRE.FindStringSubmatch(line); len(m) == 3 {
					oldName := m[1]
					newName := m[2]
					ro.UsedJoinables = append(ro.UsedJoinables, oldName)
					ro.Renames[newName] = oldName
					ro.VerifyNum++
				}

				// Duplicate data blocks → used_for_repair.
				if strings.Contains(line, "duplicate data blocks") {
					if m := targetRE.FindStringSubmatch(line); len(m) == 2 {
						ro.UsedForRepair = append(ro.UsedForRepair, m[1])
					}
				}
			} else if !inVerify {
				// Total recoverable files.
				if m := recoverableFilesRE.FindStringSubmatch(line); len(m) == 2 {
					n, _ := strconv.Atoi(m[1])
					ro.RecoverableFiles = n
				}

				if strings.HasPrefix(line, "Verifying source files:") {
					inVerify = true
					continue
				}
			} else if strings.HasPrefix(line, "Scanning extra files:") {
				// Transition: verify → extra files.
				inVerify = false
				inExtraFiles = true
				ro.Phase = PhaseExtraFiles
				ro.VerifyNum = 0
			} else {
				// In the verify phase — count target files.
				if m := targetRE.FindStringSubmatch(line); len(m) == 2 {
					ro.VerifyNum++

					// Duplicate data blocks detected during verification.
					if strings.Contains(line, "duplicate data blocks") {
						ro.UsedForRepair = append(ro.UsedForRepair, m[1])
					}
				}
			}
		}
	}

	// If no specific status was set, derive from the overall status.
	if ro.Status == StatusUnknown {
		ro.Status = parseStatus(output)
	}

	return ro
}
