package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePackageFixture writes several files into one temp directory so tests
// can exercise package-scoped behaviour, and returns the directory plus the
// path of the first file (the one to check).
//
// All files are written before any check runs: lockedFuncsInPackage memoises
// per directory, so a file added after the first call would not be seen.
func writePackageFixture(t *testing.T, files map[string]string, primary string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return filepath.Join(dir, primary)
}

func findingsFor(t *testing.T, path string) []finding {
	t.Helper()
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	return findings
}

// ---- persistence detection -------------------------------------------------

// The gap that let #227 through: a SQLite transaction running under the
// global queue mutex, with the tool having no notion of database calls.
func TestPersistence_StoreCallUnderLock(t *testing.T) {
	path := writeFixture(t, `package fixture

func (q *Queue) fail(job *Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	_ = q.store.Save(ctx, p)
}
`)
	findings := findingsFor(t, path)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for a store call under lock, got %d: %v", len(findings), findings)
	}
	if !strings.Contains(findings[0].desc, "Save") {
		t.Errorf("finding does not name the call: %q", findings[0].desc)
	}
}

func TestPersistence_DatabaseHandlesUnderLock(t *testing.T) {
	tests := []struct {
		name string
		call string
	}{
		{name: "bare db receiver", call: "_, _ = db.ExecContext(ctx, q)"},
		{name: "bare tx receiver", call: "_, _ = tx.ExecContext(ctx, q)"},
		{name: "db reached through a field", call: "_ = s.db.QueryRowContext(ctx, q)"},
		{name: "transaction begin", call: "_, _ = s.db.BeginTx(ctx, nil)"},
		{name: "commit", call: "_ = tx.Commit()"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFixture(t, `package fixture

func (s *Store) run(ctx C, q string, db D, tx T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	`+tc.call+`
}
`)
			if findings := findingsFor(t, path); len(findings) != 1 {
				t.Errorf("expected 1 finding for %q, got %d: %v", tc.call, len(findings), findings)
			}
		})
	}
}

// `store` is not an unambiguous persistence handle — internal/dirscanner uses
// it for an in-memory map — so generic accessors must not be flagged, or the
// gate becomes noise.
func TestPersistence_GenericStoreAccessorsNotFlagged(t *testing.T) {
	path := writeFixture(t, `package fixture

func (s *Scanner) check(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.store.Get(path)
	s.store.Set(path, v)
	s.store.Delete(path)
	_ = ok
}
`)
	if findings := findingsFor(t, path); len(findings) != 0 {
		t.Errorf("expected 0 findings for in-memory store accessors, got %d: %v", len(findings), findings)
	}
}

func TestPersistence_NotFlaggedOutsideLock(t *testing.T) {
	path := writeFixture(t, `package fixture

func (q *Queue) fine(job *Job) {
	q.mu.Lock()
	q.jobs = append(q.jobs, job)
	q.mu.Unlock()

	_ = q.store.Save(ctx, p)
}
`)
	if findings := findingsFor(t, path); len(findings) != 0 {
		t.Errorf("expected 0 findings when the store call follows Unlock, got %d: %v", len(findings), findings)
	}
}

func TestPersistence_TrailingMarkerSuppresses(t *testing.T) {
	path := writeFixture(t, `package fixture

func (q *Queue) deliberate(job *Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	_ = q.store.Save(ctx, p) //lockio: keeps RAM and SQLite consistent
}
`)
	if findings := findingsFor(t, path); len(findings) != 0 {
		t.Errorf("expected the trailing marker to suppress, got %d: %v", len(findings), findings)
	}
}

// The marker must be trailing. A standalone comment above the call reads as a
// suppression but is not one — the trap called out in #228, and the shape
// that sat in internal/queue/queue.go believed to be doing something.
func TestPersistence_StandaloneMarkerDoesNotSuppress(t *testing.T) {
	path := writeFixture(t, `package fixture

func (q *Queue) looksSuppressed(job *Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	//lockio: this comment is on its own line and suppresses nothing
	_ = q.store.Save(ctx, p)
}
`)
	if findings := findingsFor(t, path); len(findings) != 1 {
		t.Errorf("a standalone marker must not suppress; got %d findings: %v", len(findings), findings)
	}
}

// ---- call-graph descent ----------------------------------------------------

// The second half of #228: the lock is taken in the caller and the I/O is one
// frame down, which a single-FuncDecl walk cannot see.
func TestCallGraph_LockedCalleeInSameFile(t *testing.T) {
	path := writeFixture(t, `package fixture

func (q *Queue) caller(job *Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.handleLocked(job)
}

func (q *Queue) handleLocked(job *Job) {
	_ = os.Rename(job.Path, job.Path+".corrupt")
}
`)
	findings := findingsFor(t, path)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for I/O inside a *Locked callee, got %d: %v", len(findings), findings)
	}
	if !strings.Contains(findings[0].desc, "handleLocked") {
		t.Errorf("finding should name the callee it descended into: %q", findings[0].desc)
	}
	if !strings.Contains(findings[0].desc, "os.Rename") {
		t.Errorf("finding should name the I/O call found there: %q", findings[0].desc)
	}
}

// Package scope, not file scope: internal/downloader/tracker.go declares four
// *Locked methods that dispatch.go calls. A file-scoped map misses them all.
func TestCallGraph_LockedCalleeInSiblingFile(t *testing.T) {
	path := writePackageFixture(t, map[string]string{
		"caller.go": `package fixture

func (d *Downloader) dispatch(a *Article) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tracker.SetTriedLocked(a)
}
`,
		"tracker.go": `package fixture

func (t *Tracker) SetTriedLocked(a *Article) {
	_ = os.WriteFile(a.Path, a.Data, 0o600)
}
`,
	}, "caller.go")

	findings := findingsFor(t, path)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for I/O in a *Locked callee declared in a sibling file, got %d: %v", len(findings), findings)
	}
	if !strings.Contains(findings[0].desc, "SetTriedLocked") {
		t.Errorf("finding should name the sibling-file callee: %q", findings[0].desc)
	}
}

// The descent is keyed on the naming convention. A helper without the suffix
// carries no claim that its caller holds a lock, so descending into it would
// invent findings the convention does not support.
func TestCallGraph_NonLockedCalleeNotDescended(t *testing.T) {
	path := writeFixture(t, `package fixture

func (q *Queue) caller(job *Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.handleFailure(job)
}

func (q *Queue) handleFailure(job *Job) {
	_ = os.Rename(job.Path, job.Path+".corrupt")
}
`)
	if findings := findingsFor(t, path); len(findings) != 0 {
		t.Errorf("expected no descent into a non-*Locked callee, got %d: %v", len(findings), findings)
	}
}

func TestCallGraph_FindingAnchoredAtCallSite(t *testing.T) {
	path := writeFixture(t, `package fixture

func (q *Queue) caller(job *Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.handleLocked(job)
}

func (q *Queue) handleLocked(job *Job) {
	_ = os.Rename(job.Path, job.Path+".corrupt")
}
`)
	findings := findingsFor(t, path)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	// Line 6 is the q.handleLocked(job) call; line 10 is the os.Rename.
	// Anchoring at the call site is what makes the finding actionable and
	// lets a //lockio: marker be written where the lock is actually held.
	if findings[0].line != 6 {
		t.Errorf("finding anchored at line %d, want 6 (the call site, not the callee's I/O)", findings[0].line)
	}
}

func TestCallGraph_MarkerAtCallSiteSuppresses(t *testing.T) {
	path := writeFixture(t, `package fixture

func (q *Queue) caller(job *Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.handleLocked(job) //lockio: deliberate, tracked as a follow-up
}

func (q *Queue) handleLocked(job *Job) {
	_ = os.Rename(job.Path, job.Path+".corrupt")
}
`)
	if findings := findingsFor(t, path); len(findings) != 0 {
		t.Errorf("expected the call-site marker to suppress the descended finding, got %d: %v", len(findings), findings)
	}
}
