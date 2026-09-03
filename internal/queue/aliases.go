package queue

// DELETE ME IN TASK 13. This file is a branch-local scaffold, not a shipped
// adapter, and it does NOT make internal/queue build: the package is
// deliberately red on this branch (the content tier it depended on moved out
// from under it in Task 1/2, and ~370 call sites are going away with the
// package itself in Task 13, not being rewritten to keep it compiling in the
// meantime). What this file does is give the deleted names a place to alias
// from during the swap, so a reference to queue.Manifest et al. left in code
// this task's scope does not reach — dead code Task 13 removes wholesale —
// at least resolves to the real type rather than an undefined symbol, which
// keeps `go vet`'s error output on this package legible while it is red.
// docs/superpowers/specs/2026-08-25-job-lifecycle-design.md §15 forbids
// shipping adapters; it does not forbid a branch-local, deleted-before-merge
// one. Task 13 deletes this file and fails if it is still present.

import "github.com/hobeone/gonzbd/internal/job"

type (
	Manifest     = job.Manifest
	JobProgress  = job.JobProgress
	FileProgress = job.FileProgress
	FetchPolicy  = job.FetchPolicy
	RepairState  = job.RepairState
	JobFile      = job.JobFile
	JobArticle   = job.JobArticle
	FileMeta     = job.FileMeta
)

const (
	FetchAlways   = job.FetchAlways
	FetchIfNeeded = job.FetchIfNeeded
	FetchNever    = job.FetchNever

	RepairIntact         = job.RepairIntact
	RepairPossible       = job.RepairPossible
	RepairUnknown        = job.RepairUnknown
	RepairNoCapacity     = job.RepairNoCapacity
	RepairBeyondCapacity = job.RepairBeyondCapacity
)

var (
	AllFetchPolicies = job.AllFetchPolicies
	AllRepairStates  = job.AllRepairStates
	RepairStateFrom  = job.RepairStateFrom
)
