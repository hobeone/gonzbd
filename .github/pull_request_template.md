## Description
<!-- Provide a brief summary of the changes and the motivation behind them.
Describe the problem being solved, the approach taken and the material landed
improvements that this PR brings. -->

## Checklist
- [ ] I have run `go vet ./...` and `golangci-lint run ./...`
- [ ] I have run `go test -race ./...` and all tests passed
- [ ] **I have updated the state diagram in `docs/implementation_notes.md`** if these changes touch state-mutating queue or app methods.
- [ ] **Every intentional snapshot-then-release lock pattern has a `// --- No lock held below this line ---` comment** (AGENTS.md § Concurrency & Locking) if this PR unlocks a mutex mid-function rather than via `defer`.

## Related Issues
<!-- Link to any related issues or the hardening plan step. -->
