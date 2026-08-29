package dispatch

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package's tests if any goroutine outlives them. This
// package now owns a real goroutine (run, launched by Start) — the same
// reason internal/app, internal/assembler and internal/downloader each carry
// this TestMain.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
