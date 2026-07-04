package postproc

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
)

// VerifiedSets tracks which par2 sets have been successfully verified/repaired.
// Persisted to disk so crash restarts skip already-verified sets.
type VerifiedSets struct {
	mu   sync.Mutex
	sets map[string]bool // set name → verified
	path string          // abs path to __verified__ file
	log  *slog.Logger
}

// NewVerifiedSets loads (or creates) the verified sets file for a job.
// downloadDir is the job's download directory.
func NewVerifiedSets(downloadDir string, log *slog.Logger) *VerifiedSets {
	adminDir := filepath.Join(downloadDir, constants.JobAdminDirName)
	path := filepath.Join(adminDir, constants.VerifiedFileName)
	if log == nil {
		log = slog.Default().With("component", "postproc")
	}
	vs := &VerifiedSets{
		sets: make(map[string]bool),
		path: path,
		log:  log,
	}
	vs.load()
	return vs
}

// IsVerified reports whether the named set has been successfully verified.
func (v *VerifiedSets) IsVerified(setName string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.sets[setName]
}

// AllVerified reports whether ALL sets are verified. Returns false if no sets recorded.
func (v *VerifiedSets) AllVerified() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.sets) == 0 {
		return false
	}
	for _, ok := range v.sets {
		if !ok {
			return false
		}
	}
	return true
}

// MarkVerified records a set as verified (true) or failed (false) and persists.
// Logs a warning if the state cannot be saved to disk; the in-memory state
// is always updated regardless.
func (v *VerifiedSets) MarkVerified(setName string, ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sets[setName] = ok
	if err := v.save(); err != nil {
		v.log.Warn("verified: failed to persist par2 verification state",
			"path", v.path, "err", err)
	}
}

func (v *VerifiedSets) load() {
	data, err := os.ReadFile(v.path)
	if err != nil {
		return // no file yet, start fresh
	}
	if err := json.Unmarshal(data, &v.sets); err != nil {
		v.log.Warn("verified: failed to unmarshal par2 verification state",
			"path", v.path, "err", err)
	}
}

func (v *VerifiedSets) save() error {
	dir := filepath.Dir(v.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	data, err := json.Marshal(v.sets)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return fsutil.WriteAtomicBytes(v.path, data)
}
