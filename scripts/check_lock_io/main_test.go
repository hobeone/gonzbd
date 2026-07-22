package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFixture writes src to a temp file and returns its path.
func writeFixture(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// TestClosureWrapper_LogOutsideClosure is PR #125's fixed shape: the log
// call sits after WithRead returns, not inside its closure. Must not be
// flagged.
func TestClosureWrapper_LogOutsideClosure(t *testing.T) {
	path := writeFixture(t, `package fixture

func trustedFn(cfg *Config, log *Logger) {
	var trusted bool
	cfg.WithRead(func(c *Config) {
		trusted = c.Check()
	})
	if !trusted {
		log.Warn("refused", "reason", "not trusted")
	}
}
`)
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings for the fixed shape (log call is outside the closure), got %d: %v", len(findings), findings)
	}
}

// TestClosureWrapper_LogInsideClosure reconstructs PR #125's original bug:
// the log call lexically inside the WithRead closure. Must be flagged.
func TestClosureWrapper_LogInsideClosure(t *testing.T) {
	path := writeFixture(t, `package fixture

func trustedFn(cfg *Config, log *Logger) bool {
	var trusted bool
	cfg.WithRead(func(c *Config) {
		trusted = c.Check()
		if !trusted {
			log.Warn("refused", "reason", "not trusted")
		}
	})
	return trusted
}
`)
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for log call inside WithRead closure, got %d: %v", len(findings), findings)
	}
}

// TestManualUnlock_PreFix reconstructs events.go's Broadcast before its fix:
// a log call between RLock and the manual (non-deferred) RUnlock. Must be
// flagged — this specifically validates the non-defer detection path.
func TestManualUnlock_PreFix(t *testing.T) {
	path := writeFixture(t, `package fixture

func (b *Broadcaster) Broadcast(event Event) {
	b.mu.RLock()
	if len(b.clients) > 0 {
		b.log.Debug("broadcast", "event", event.Type)
	}
	b.mu.RUnlock()
}
`)
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for log call inside manual RLock/RUnlock span, got %d: %v", len(findings), findings)
	}
}

// TestManualUnlock_PostFix is the corrected shape: log call after the
// manual RUnlock. Must not be flagged.
func TestManualUnlock_PostFix(t *testing.T) {
	path := writeFixture(t, `package fixture

func (b *Broadcaster) Broadcast(event Event) {
	b.mu.RLock()
	numClients := len(b.clients)
	b.mu.RUnlock()
	if numClients > 0 {
		b.log.Debug("broadcast", "event", event.Type)
	}
}
`)
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings for the fixed shape, got %d: %v", len(findings), findings)
	}
}

// TestDeferUnlock_LoggingAfterReturn is the dominant idiom: Lock() followed
// immediately by defer Unlock(), with an I/O call later in the function.
// Must be flagged (the deferred unlock only fires at return, so the I/O
// call genuinely runs with the lock held).
func TestDeferUnlock_LoggingAfterReturn(t *testing.T) {
	path := writeFixture(t, `package fixture

func (a *App) SetDir(dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dir = dir
	a.log.Info("dir updated", "dir", dir)
}
`)
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for log call after Lock()+defer Unlock(), got %d: %v", len(findings), findings)
	}
}

// TestDeferRLock_LoggingAfterReturn mirrors TestDeferUnlock_LoggingAfterReturn
// but for the read-lock variant (RLock()+defer RUnlock()), exercising
// matchingLockKind's RLock<->RUnlock branch specifically — the write-lock
// case alone doesn't cover this.
func TestDeferRLock_LoggingAfterReturn(t *testing.T) {
	path := writeFixture(t, `package fixture

func (a *App) Status() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s := a.state
	a.log.Info("status read", "state", s)
	return s
}
`)
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for log call after RLock()+defer RUnlock(), got %d: %v", len(findings), findings)
	}
}

// TestDeferInSwitchCase_ExtendsToFunctionEnd is a regression test for a
// false negative found during review: a Lock()+defer Unlock() pair starting
// as a switch case's first statements was invisible to the deferred-lock
// detector, because case clause bodies are a raw []ast.Stmt, not wrapped in
// *ast.BlockStmt. The deferred unlock still only fires at the enclosing
// function's return, so a log call after the switch is still under lock.
func TestDeferInSwitchCase_ExtendsToFunctionEnd(t *testing.T) {
	path := writeFixture(t, `package fixture

func (d *D) run(kind int) {
	switch kind {
	case 1:
		d.mu.Lock()
		defer d.mu.Unlock()
	}
	d.log.Info("after switch, lock still held via defer")
}
`)
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for log call after a switch-case defer lock, got %d: %v", len(findings), findings)
	}
}

// TestDeferInSelectCase_ExtendsToFunctionEnd mirrors
// TestDeferInSwitchCase_ExtendsToFunctionEnd for the select-statement case
// (ast.CommClause), the other raw-[]ast.Stmt shape blockList() was fixed to
// recognize alongside ast.CaseClause.
func TestDeferInSelectCase_ExtendsToFunctionEnd(t *testing.T) {
	path := writeFixture(t, `package fixture

func (d *D) run(ch chan int) {
	select {
	case <-ch:
		d.mu.Lock()
		defer d.mu.Unlock()
	}
	d.log.Info("after select, lock still held via defer")
}
`)
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for log call after a select-case defer lock, got %d: %v", len(findings), findings)
	}
}

// TestLabeledLoop_StillScanned is a regression test for a false negative
// found during review: recurseNested had no case for *ast.LabeledStmt (used
// with labeled break/continue, e.g. `retry: for {...}`), so a labeled loop
// inside a locked span was skipped entirely, along with any I/O call inside
// it.
func TestLabeledLoop_StillScanned(t *testing.T) {
	path := writeFixture(t, `package fixture

func (d *D) run() {
	d.mu.Lock()
retry:
	for i := 0; i < 3; i++ {
		if i == 1 {
			continue retry
		}
		d.log.Info("inside labeled loop")
	}
	d.mu.Unlock()
}
`)
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for log call inside a labeled loop under lock, got %d: %v", len(findings), findings)
	}
}

// TestClosureLock_DoesNotLeakToOuterScope is a regression test for a false
// positive found during review: collectDeferredLocks walked into a goroutine
// literal's body unconditionally, so a Lock()+defer Unlock() pair entirely
// inside a `go func(){...}()` was incorrectly treated as covering unrelated
// code later in the enclosing function too.
func TestClosureLock_DoesNotLeakToOuterScope(t *testing.T) {
	path := writeFixture(t, `package fixture

func (t *T) run() {
	go func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.doWork()
	}()

	os.ReadFile("/tmp/unrelated")
}
`)
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings — the closure's lock must not leak to the outer function, got %d: %v", len(findings), findings)
	}
}

// TestSuppressionComment_MultiLineCall is a regression test for a bug found
// during review: report() checked commentsByLine using only the call's
// start line, so a //lockio: comment on a multi-line call's last line (the
// natural place for it, after the closing paren) was never seen.
func TestSuppressionComment_MultiLineCall(t *testing.T) {
	path := writeFixture(t, `package fixture

func (d *D) run() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log.Info(
		"multi-line call",
		"key", "value",
	) //lockio: intentional, suppress this
}
`)
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings — //lockio: on a multi-line call's last line should suppress, got %d: %v", len(findings), findings)
	}
}

// TestEarlyReturnBranch_DoesNotLeak verifies the copy-on-recurse design:
// an early "unlock, log, return" branch (the repo's own correct idiom) must
// not cause a false positive on sibling statements after the branch, and a
// nested unlock+log must not itself be flagged.
func TestEarlyReturnBranch_DoesNotLeak(t *testing.T) {
	path := writeFixture(t, `package fixture

func (o *Orchestrator) Add(limit int) {
	o.mu.Lock()
	if o.active >= limit {
		o.mu.Unlock()
		o.log.Debug("skipping, limit reached")
		return
	}
	o.active++
	o.mu.Unlock()
}
`)
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings for the early-return-then-log idiom, got %d: %v", len(findings), findings)
	}
}

// TestErrError_NotFlagged is the false-positive regression case found
// during implementation: err.Error() (the stdlib error interface method,
// zero arguments) must not be confused with a slog.Logger.Error(...) call.
func TestErrError_NotFlagged(t *testing.T) {
	path := writeFixture(t, `package fixture

func (d *Unpacker) run(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.recordFailure(d.setname, err.Error())
}
`)
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings — err.Error() is not a logging call, got %d: %v", len(findings), findings)
	}
}

// TestSuppressionComment verifies //lockio: suppresses an otherwise-real
// finding.
func TestSuppressionComment(t *testing.T) {
	path := writeFixture(t, `package fixture

func (a *App) SetDir(dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dir = dir
	a.log.Info("dir updated", "dir", dir) //lockio: intentional, see doc comment
}
`)
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings — //lockio: comment should suppress, got %d: %v", len(findings), findings)
	}
}

// TestNntpDial verifies the project-specific nntp.Dial network call is
// detected, matching the gap found and fixed during implementation.
func TestNntpDial(t *testing.T) {
	path := writeFixture(t, `package fixture

func (m *managedConn) Get() {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, _ := nntp.Dial(ctx, cfg)
	m.conn = c
}
`)
	findings, err := checkFile(path)
	if err != nil {
		t.Fatalf("checkFile: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for nntp.Dial under lock, got %d: %v", len(findings), findings)
	}
}
