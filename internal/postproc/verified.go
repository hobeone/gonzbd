package postproc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/hobeone/gonzbd/internal/constants"
)

// VerifiedSets tracks which par2 sets have been successfully verified/repaired.
// Persisted to disk so crash restarts skip already-verified sets.
type VerifiedSets struct {
	mu   sync.Mutex
	sets map[string]bool // set name → verified
	path string          // abs path to __verified__ file
}

// NewVerifiedSets loads (or creates) the verified sets file for a job.
// downloadDir is the job's download directory.
func NewVerifiedSets(downloadDir string) *VerifiedSets {
	adminDir := filepath.Join(downloadDir, constants.JobAdminDirName)
	path := filepath.Join(adminDir, constants.VerifiedFileName)
	vs := &VerifiedSets{
		sets: make(map[string]bool),
		path: path,
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
func (v *VerifiedSets) MarkVerified(setName string, ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sets[setName] = ok
	v.save()
}

func (v *VerifiedSets) load() {
	data, err := os.ReadFile(v.path)
	if err != nil {
		return // no file yet, start fresh
	}
	_ = json.Unmarshal(data, &v.sets)
}

func (v *VerifiedSets) save() {
	dir := filepath.Dir(v.path)
	_ = os.MkdirAll(dir, 0o750)
	data, err := json.Marshal(v.sets)
	if err != nil {
		return
	}
	// Atomic write: temp → rename.
	tmp, err := os.CreateTemp(dir, "verified-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	tmp.Close()
	_ = os.Rename(tmpName, v.path)
}
