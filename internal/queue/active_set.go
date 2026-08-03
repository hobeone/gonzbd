package queue

import (
	"sync"
)

// ActiveSet manages in-memory resident (Manifest, JobProgress) pairs exclusively
// for active downloading and post-processing jobs.
type ActiveSet struct {
	mu        sync.RWMutex
	resident  map[string]*Job
	maxActive int
}

// NewActiveSet constructs an ActiveSet bounded by maxActive (defaulting to 4 if <= 0).
func NewActiveSet(maxActive int) *ActiveSet {
	if maxActive <= 0 {
		maxActive = 4
	}
	return &ActiveSet{
		resident:  make(map[string]*Job),
		maxActive: maxActive,
	}
}

// Len returns the count of currently resident jobs in memory.
func (a *ActiveSet) Len() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.resident)
}

// MaxActive returns the maximum allowed active resident jobs.
func (a *ActiveSet) MaxActive() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.maxActive
}

// SetMaxActive updates the maximum active job limit.
func (a *ActiveSet) SetMaxActive(maxActive int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if maxActive <= 0 {
		maxActive = 4
	}
	a.maxActive = maxActive
}

// IsResident reports whether the job ID is currently resident in memory.
func (a *ActiveSet) IsResident(id string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.resident[id]
	return ok
}

// Add registers a job into the active set after its manifest/progress are loaded.
func (a *ActiveSet) Add(job *Job) {
	if job == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resident[job.ID] = job
}

// Evict removes a job from the active set.
func (a *ActiveSet) Evict(job *Job) {
	if job == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.resident, job.ID)
}

// Clear evicts all resident jobs from the active set.
func (a *ActiveSet) Clear() {
	a.mu.Lock()
	defer a.mu.Unlock()
	clear(a.resident)
}
