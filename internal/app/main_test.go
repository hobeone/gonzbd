package app

import (
	"log/slog"
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package's tests if any goroutine outlives them.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	goleak.VerifyTestMain(m,
		// database/sql starts these per DB handle in OpenDB and stops them
		// only on db.Close(). Tests that construct a history repo without
		// closing it leave them parked in select. Production closes the DB in
		// serveMode's deferred cleanup, so these are test-scope only.
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionOpener"),
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionCleaner"),
	)
}
