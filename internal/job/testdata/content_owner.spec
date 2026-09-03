# The red check for the content tier's owner model (AGENTS.md Rule 2).
#
#     go run ./scripts/mutate internal/job/testdata/content_owner.spec
#
# Every mutation neuters one condition the owner rests on, and the tests named
# below must die when it does. Conditions are neutered rather than blocks
# deleted: a deletion usually breaks the build, and COMPILE_ERROR is not
# evidence that a test discriminates.
#
# The run filter is one alternation because the tool takes a single global
# `run` directive; each mutation is still killed by the one test written for
# it, and the alternation only keeps the other three from running needlessly.
pkg ./internal/job/
run TestAttachContent_IsTheSoleConstructorOfThePair|TestEvict_KeepsProgressAndTheScalars|TestArticleDoors_RefuseANonResidentJob|TestRestoreContent_RefusesAMismatchedPair

[AttachContent stops deriving progress and leaves it nil]
file internal/job/content.go
--- anchor
	p := newJobProgress(m)
--- replace
	var p *JobProgress
--- end

[Manifest returns nil,nil instead of ErrNotResident]
file internal/job/content.go
--- anchor
		return nil, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
--- replace
		return nil, nil
--- end

[AttachContent installs the manifest without its derived scalars]
file internal/job/content.go
--- anchor
	j.totalBytes = m.TotalBytes()
--- replace
	j.totalBytes = 0
--- end

[Evict drops the always-resident progress tier too]
file internal/job/content.go
--- anchor
func (j *Job) Evict() {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	j.manifest = nil
}
--- replace
func (j *Job) Evict() {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	j.manifest = nil
	j.progress = nil
}
--- end

[the residency gate on the article doors is neutered]
file internal/job/content.go
--- anchor
	if j.manifest == nil || j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	return fn(j.manifest, j.progress)
--- replace
	if j.manifest == nil || j.progress == nil {
		return nil
	}
	return fn(j.manifest, j.progress)
--- end

[RestoreContent trusts the caller's pair instead of checking it]
file internal/job/content.go
--- anchor
	if !p.describesSameJobAs(m) {
--- replace
	if false {
--- end
