package constants

import (
	"testing"
	"time"
)

// These tests pin numeric and string values against the upstream Python
// reference. They are intentionally brittle: any change to a Python-mirrored
// constant should require an explicit update here, drawing reviewer
// attention to the wire-format / behavior implication.

func TestPriorityValues(t *testing.T) {
	tests := []struct {
		name string
		got  Priority
		want int8
	}{
		{"RepairPriority", RepairPriority, 3},
		{"ForcePriority", ForcePriority, 2},
		{"HighPriority", HighPriority, 1},
		{"NormalPriority", NormalPriority, 0},
		{"LowPriority", LowPriority, -1},
		{"PausedPriority", PausedPriority, -2},
		{"StopPriority", StopPriority, -4},
		{"DefaultPriority", DefaultPriority, -100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if int8(tc.got) != tc.want {
				t.Fatalf("%s = %d, want %d", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestPriorityOrdering(t *testing.T) {
	// Active priorities (everything except PausedPriority/StopPriority/DefaultPriority)
	// must form a strict descending sequence.
	ordered := []Priority{
		RepairPriority,
		ForcePriority,
		HighPriority,
		NormalPriority,
		LowPriority,
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i] >= ordered[i-1] {
			t.Fatalf("priority order violated at index %d: %d >= %d",
				i, ordered[i], ordered[i-1])
		}
	}
}

func TestPriorityString(t *testing.T) {
	tests := map[Priority]string{
		RepairPriority:  "Repair",
		ForcePriority:   "Force",
		HighPriority:    "High",
		NormalPriority:  "Normal",
		LowPriority:     "Low",
		PausedPriority:  "Paused",
		StopPriority:    "Stop",
		DefaultPriority: "Default",
		Priority(7):     "Priority(7)",
		Priority(-3):    "Priority(-3)",
	}
	for p, want := range tests {
		t.Run(want, func(t *testing.T) {
			if got := p.String(); got != want {
				t.Fatalf("Priority(%d).String() = %q, want %q", p, got, want)
			}
		})
	}
}

func TestStatusValues(t *testing.T) {
	// Every status string must match the upstream Python sabnzbd Status
	// class verbatim — third-party clients pattern-match on these.
	tests := map[Status]string{
		StatusIdle:        "Idle",
		StatusQueued:      "Queued",
		StatusGrabbing:    "Grabbing",
		StatusFetching:    "Fetching",
		StatusDownloading: "Downloading",
		StatusPaused:      "Paused",
		StatusPropagating: "Propagating",
		StatusChecking:    "Checking",
		StatusQuickCheck:  "QuickCheck",
		StatusVerifying:   "Verifying",
		StatusRepairing:   "Repairing",
		StatusExtracting:  "Extracting",
		StatusMoving:      "Moving",
		StatusRunning:     "Running",
		StatusCompleted:   "Completed",
		StatusFailed:      "Failed",
		StatusDeleted:     "Deleted",
	}
	for s, want := range tests {
		t.Run(want, func(t *testing.T) {
			if string(s) != want {
				t.Fatalf("Status %q != %q", string(s), want)
			}
		})
	}
}

func TestAllStatusesIsExhaustive(t *testing.T) {
	if got, want := len(AllStatuses), 17; got != want {
		t.Fatalf("AllStatuses has %d entries, want %d (update both lists in tandem)", got, want)
	}
	seen := make(map[Status]bool, len(AllStatuses))
	for _, s := range AllStatuses {
		if seen[s] {
			t.Errorf("duplicate status in AllStatuses: %q", s)
		}
		seen[s] = true
	}
}

func TestDuplicateStatusValues(t *testing.T) {
	tests := map[DuplicateStatus]string{
		DuplicateExact:            "Duplicate",
		DuplicateAlternative:      "Duplicate Alternative",
		SmartDuplicate:            "Smart Duplicate",
		SmartDuplicateAlternative: "Smart Duplicate Alternative",
		DuplicateIgnored:          "Duplicate Ignored",
	}
	for s, want := range tests {
		t.Run(want, func(t *testing.T) {
			if string(s) != want {
				t.Fatalf("DuplicateStatus %q != %q", string(s), want)
			}
		})
	}
}

func TestAddNzbFileResultValues(t *testing.T) {
	tests := map[AddNzbFileResult]string{
		AddRetry:            "Retry",
		AddError:            "Error",
		AddPrequeueRejected: "Pre-queue rejected",
		AddOK:               "OK",
		AddNoFilesFound:     "No files found",
	}
	for r, want := range tests {
		t.Run(want, func(t *testing.T) {
			if string(r) != want {
				t.Fatalf("AddNzbFileResult %q != %q", string(r), want)
			}
		})
	}
}

func TestPenaltyDurations(t *testing.T) {
	// Match spec §3.6 / Python sabnzbd/downloader.py exactly.
	tests := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"PenaltyUnknown", PenaltyUnknown, 3 * time.Minute},
		{"PenaltyVeryShort", PenaltyVeryShort, 6 * time.Second},
		{"PenaltyShort", PenaltyShort, 1 * time.Minute},
		{"Penalty502", Penalty502, 5 * time.Minute},
		{"PenaltyTimeout", PenaltyTimeout, 10 * time.Minute},
		{"PenaltyShare", PenaltyShare, 10 * time.Minute},
		{"PenaltyTooMany", PenaltyTooMany, 10 * time.Minute},
		{"PenaltyPerm", PenaltyPerm, 10 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s = %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}

	if OptionalDeactivationThreshold != 0.3 {
		t.Fatalf("OptionalDeactivationThreshold = %v, want 0.3", OptionalDeactivationThreshold)
	}
}

func TestBinaryUnits(t *testing.T) {
	tests := []struct {
		name string
		got  int64
		want int64
	}{
		{"KiB", KiB, 1024},
		{"MiB", MiB, 1024 * 1024},
		{"GiB", GiB, 1024 * 1024 * 1024},
		{"TiB", TiB, 1024 * 1024 * 1024 * 1024},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s = %d, want %d", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestCacheLimits(t *testing.T) {
	if DefaultArticleCacheBytes != 500*MiB {
		t.Errorf("DefaultArticleCacheBytes = %d, want %d", DefaultArticleCacheBytes, 500*MiB)
	}
	if MaxArticleCacheBytes != GiB {
		t.Errorf("MaxArticleCacheBytes = %d, want %d", MaxArticleCacheBytes, GiB)
	}
	if ArticleCacheNonContiguousFlushPercentage != 0.9 {
		t.Errorf("ArticleCacheNonContiguousFlushPercentage = %v, want 0.9", ArticleCacheNonContiguousFlushPercentage)
	}
	if DefaultArticleCacheBytes > MaxArticleCacheBytes {
		t.Errorf("default cache (%d) > max cache (%d)", DefaultArticleCacheBytes, MaxArticleCacheBytes)
	}
}

func TestAssemblerLimits(t *testing.T) {
	if MaxAssemblerQueue != 12 {
		t.Errorf("MaxAssemblerQueue = %d, want 12", MaxAssemblerQueue)
	}
	if SoftAssemblerQueueLimit != 0.5 {
		t.Errorf("SoftAssemblerQueueLimit = %v, want 0.5", SoftAssemblerQueueLimit)
	}
	if AssemblerTriggerPercentage != 0.05 {
		t.Errorf("AssemblerTriggerPercentage = %v, want 0.05", AssemblerTriggerPercentage)
	}
	if AssemblerDelayFactorDirectWrite != 1.5 {
		t.Errorf("AssemblerDelayFactorDirectWrite = %v, want 1.5", AssemblerDelayFactorDirectWrite)
	}
	if AssemblerWriteInterval != 5*time.Second {
		t.Errorf("AssemblerWriteInterval = %v, want 5s", AssemblerWriteInterval)
	}
}

func TestNetworkBuffers(t *testing.T) {
	if NNTPBufferSize != 256*1024 {
		t.Errorf("NNTPBufferSize = %d, want %d", NNTPBufferSize, 256*1024)
	}
	if NNTPMaxBufferSize != 10*1024*1024 {
		t.Errorf("NNTPMaxBufferSize = %d, want %d", NNTPMaxBufferSize, 10*1024*1024)
	}
	if DefaultPipeliningRequests != 2 {
		t.Errorf("DefaultPipeliningRequests = %d, want 2", DefaultPipeliningRequests)
	}
	if NNTPBufferSize > NNTPMaxBufferSize {
		t.Errorf("NNTPBufferSize (%d) > NNTPMaxBufferSize (%d)", NNTPBufferSize, NNTPMaxBufferSize)
	}
}

func TestNetworkTimeouts(t *testing.T) {
	tests := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"DefaultNetworkingTimeout", DefaultNetworkingTimeout, 60 * time.Second},
		{"DefaultNetworkingTestTimeout", DefaultNetworkingTestTimeout, 5 * time.Second},
		{"DefaultNetworkingShortTimeout", DefaultNetworkingShortTimeout, 3 * time.Second},
		{"DefaultDirScanRate", DefaultDirScanRate, 5 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s = %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestFilesystemNameLimits(t *testing.T) {
	if MaxFolderNameLen != 246 {
		t.Errorf("MaxFolderNameLen = %d, want 246 (256-10)", MaxFolderNameLen)
	}
	if MaxFileNameLen != 245 {
		t.Errorf("MaxFileNameLen = %d, want 245 (255-10)", MaxFileNameLen)
	}
	if MaxFileExtensionLen != 20 {
		t.Errorf("MaxFileExtensionLen = %d, want 20", MaxFileExtensionLen)
	}
}

func TestWarningCounters(t *testing.T) {
	if MaxWarnings != 20 {
		t.Errorf("MaxWarnings = %d, want 20", MaxWarnings)
	}
	if MaxBadArticles != 5 {
		t.Errorf("MaxBadArticles = %d, want 5", MaxBadArticles)
	}
}

func TestPersistenceNames(t *testing.T) {
	tests := map[string]string{
		"AdminDirName":     AdminDirName,
		"JobAdminDirName":  JobAdminDirName,
		"VerifiedFileName": VerifiedFileName,
		"RenamesFileName":  RenamesFileName,
		"AttribFileName":   AttribFileName,
	}
	want := map[string]string{
		"AdminDirName":     "admin",
		"JobAdminDirName":  "__ADMIN__",
		"VerifiedFileName": "__verified__",
		"RenamesFileName":  "__renames__",
		"AttribFileName":   "SABnzbd_attrib",
	}
	for name, got := range tests {
		t.Run(name, func(t *testing.T) {
			if got != want[name] {
				t.Fatalf("%s = %q, want %q", name, got, want[name])
			}
		})
	}
}
