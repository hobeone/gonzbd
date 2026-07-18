package app

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package's tests if any goroutine outlives them.
//
// internal/app owns the longest-lived goroutines in the daemon (pipeline,
// watchCompletions, runCheckpoint, runMetricsPush, plus the assembler and
// downloader workers it starts), and Shutdown's ordering is load-bearing:
// cancelling a context is not sufficient to stop the assembler worker, which
// Shutdown drains explicitly. Leak detection is what keeps that ordering
// honest — see issues #92 and #104.
//
// Each ignore below is a goroutine owned by a dependency and torn down by
// closing that dependency, not by anything this package controls.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// database/sql starts these per DB handle in OpenDB and stops them
		// only on db.Close(). Tests that construct a history repo without
		// closing it leave them parked in select. Production closes the DB in
		// serveMode's deferred cleanup, so these are test-scope only.
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionOpener"),
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionCleaner"),
		goleak.IgnoreTopFunction("github.com/hobeone/gonzbd/internal/app_test.(*wedgedDownloader).Stop"),
	)
}
