package par2

import (
	"os"
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

	// DamagedTargets lists target filenames that par2 reported as damaged or
	// missing during initial verification ("Target: \"X\" - damaged." or
	// "Target: \"X\" - missing."). RepairStage uses this to identify which
	// ".1"/".2" backup files par2 created during repair.
	DamagedTargets []string

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

	for rawLine := range strings.FieldsFuncSeq(output, func(r rune) bool { return r == '\n' || r == '\r' }) {
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

		if parseGlobalLine(line, ro, &verified) {
			continue
		}

		if parseOptionOrValidity(line, ro, verified) {
			continue
		}

		if parseRepairProgress(line, ro) {
			continue
		}

		if parsePhaseLine(line, ro, &inVerify, &inExtraFiles) {
			continue
		}

		if !verified {
			parseFinalStateOrFailure(line, ro, inVerify, inExtraFiles)
		}
	}

	// If no specific status was set, derive from the overall status.
	if ro.Status == StatusUnknown {
		ro.Status = parseStatus(output)
	}

	return ro
}

// parseGlobalLine checks lines that don't depend on phase or verify state.
func parseGlobalLine(line string, ro *RepairOutput, verified *bool) bool {
	if strings.HasPrefix(line, "Invalid option specified") ||
		strings.HasPrefix(line, "Invalid thread option") ||
		strings.HasPrefix(line, "Cannot specify recovery file count") {
		ro.InvalidOption = true
		ro.Status = StatusBadOption
		return true
	}
	if strings.HasPrefix(line, "All files are correct") {
		ro.Finished = true
		ro.Status = StatusAllFilesOK
		return true
	}
	if strings.HasPrefix(line, "Repair is required") {
		ro.Status = StatusRepairRequired
		*verified = true
		ro.VerifyTotal = 0
		ro.VerifyNum = 0
		return true
	}
	if strings.HasPrefix(line, "Main packet not found") ||
		strings.Contains(line, "The recovery file does not exist") {
		ro.Status = StatusInvalidPar2
		return true
	}
	return false
}

// parseOptionOrValidity parses option errors, missing files count, disk space and renamed errors.
func parseOptionOrValidity(line string, ro *RepairOutput, verified bool) bool {
	if strings.HasPrefix(line, "You need") {
		if m := needMoreBlocksRE.FindStringSubmatch(line); len(m) == 2 {
			ro.NeedMoreBlocks = true
			//nolint:errcheck // non-numeric token fallback handled by default 0
			ro.BlocksNeeded, _ = strconv.Atoi(m[1])
			ro.Status = StatusNeedMoreBlocks
		}
		return true
	}
	if verified && (strings.HasSuffix(line, "are missing.") || strings.HasSuffix(line, "exist but are damaged.")) {
		if m := missingDamagedRE.FindStringSubmatch(line); len(m) == 2 {
			//nolint:errcheck // non-numeric token fallback handled by default 0
			n, _ := strconv.Atoi(m[1])
			ro.VerifyTotal += n
		}
		return true
	}
	if strings.Contains(line, "cannot be renamed to") {
		ro.Status = StatusRepairNotPossible
		return true
	}
	if strings.Contains(line, "not enough space on the disk") ||
		strings.Contains(line, "Insufficient disk space") {
		ro.Status = StatusDiskFull
		return true
	}
	if strings.HasPrefix(line, "No details available for recoverable file") {
		ro.Status = StatusRepairNotPossible
		return true
	}
	return false
}

// parseRepairProgress parses repair status, percentages and command completion.
func parseRepairProgress(line string, ro *RepairOutput) bool {
	if strings.HasPrefix(line, "Repair is possible") {
		ro.Status = StatusRepairPossible
		ro.Phase = PhaseRepairing
		ro.RepairPercent = 0
		ro.RepairStarted = time.Now()
		return true
	}
	if strings.HasPrefix(line, "Repair is not possible") {
		ro.Status = StatusRepairNotPossible
		return true
	}
	if m := repairProgressRE.FindStringSubmatch(line); len(m) == 2 {
		if pct, err := strconv.ParseFloat(m[1], 64); err == nil {
			ro.RepairPercent = pct
		}
		return true
	}
	if strings.HasPrefix(line, "Repair complete") {
		ro.Finished = true
		if ro.Status != StatusRepairFailed && ro.Status != StatusRepairNotPossible {
			ro.Status = StatusRepairComplete
		}
		return true
	}
	if strings.HasPrefix(line, "Repair Failed") {
		ro.Status = StatusRepairFailed
		ro.Finished = false
		return true
	}
	return false
}

// parsePhaseLine handles transition lines like Verifying repaired files, Scanning extra files.
func parsePhaseLine(line string, ro *RepairOutput, inVerify, inExtraFiles *bool) bool {
	if strings.HasPrefix(line, "Verifying repaired files") {
		ro.Phase = PhaseVerifyingRepair
		return true
	}
	if ro.Phase == PhaseVerifyingRepair && strings.HasPrefix(line, "Target") {
		ro.VerifyNum++
		return true
	}
	if strings.Contains(line, "Could not write") && strings.Contains(line, "at offset 0:") {
		ro.Finished = true
		return true
	}
	if strings.HasPrefix(line, "Scanning source files:") || strings.HasPrefix(line, "Verifying source files:") {
		*inVerify = true
		return true
	}
	if strings.HasPrefix(line, "Scanning extra files:") {
		*inVerify = false
		*inExtraFiles = true
		ro.Phase = PhaseExtraFiles
		ro.VerifyNum = 0
		return true
	}
	return false
}

// parseFinalStateOrFailure checks scan-time target blocks and matches.
func parseFinalStateOrFailure(line string, ro *RepairOutput, inVerify, inExtraFiles bool) {
	// "Scanning:" lines are silently consumed.
	if strings.HasPrefix(line, "Scanning:") {
		return
	}

	switch {
	case inExtraFiles:
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
	case !inVerify:
		// Total recoverable files.
		if m := recoverableFilesRE.FindStringSubmatch(line); len(m) == 2 {
			//nolint:errcheck // non-numeric token fallback handled by default 0
			n, _ := strconv.Atoi(m[1])
			ro.RecoverableFiles = n
		}
	default:
		// In the verify phase — count target files.
		if m := targetRE.FindStringSubmatch(line); len(m) == 2 {
			ro.VerifyNum++

			if !strings.HasSuffix(line, "- found.") && !strings.Contains(line, "- found.") {
				ro.DamagedTargets = append(ro.DamagedTargets, m[1])
			}

			// Duplicate data blocks detected during verification.
			if strings.Contains(line, "duplicate data blocks") {
				ro.UsedForRepair = append(ro.UsedForRepair, m[1])
			}
		}
	}
}

// IsBackupNumber reports whether suffix is a par2 backup index ("1".."999",
// non-zero-padded) as emitted by par2cmdline when renaming a damaged file to
// "<target>.1", "<target>.2", etc.
func IsBackupNumber(suffix string) bool {
	if suffix == "" || len(suffix) > 3 || suffix[0] < '1' || suffix[0] > '9' {
		return false
	}
	for i := 1; i < len(suffix); i++ {
		if suffix[i] < '0' || suffix[i] > '9' {
			return false
		}
	}
	return true
}

// IsBackupOfVerifiedTarget reports whether candidateName is a "<targetName>.<N>"
// par2 backup file alongside an existing regular file targetName whose 16 KiB
// MD5 equals wantHash (#768).
func IsBackupOfVerifiedTarget(root *os.Root, candidateName, targetName string, wantHash [16]byte) bool {
	suffix, ok := strings.CutPrefix(candidateName, targetName+".")
	if !ok || !IsBackupNumber(suffix) {
		return false
	}
	info, err := root.Lstat(targetName)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	targetHash, err := ComputeHash16kRoot(root, targetName)
	return err == nil && targetHash == wantHash
}
