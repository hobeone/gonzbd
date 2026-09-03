package queue

// DELETE ME IN TASK 13. This file is a branch-local scaffold, not a shipped
// adapter: it exists so every commit between Task 1 and Task 13 builds, which
// AGENTS.md requires and which a red-green check needs in order to run at all.
// docs/superpowers/specs/2026-08-25-job-lifecycle-design.md §15 forbids
// shipping adapters; it does not forbid an intermediate commit. Task 13 deletes
// this file and fails if it is still present.

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
)

var (
	RepairStateFrom = job.RepairStateFrom
)
