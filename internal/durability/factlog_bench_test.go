package durability

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/history"
)

// openBenchDB opens a migrated database with the PRODUCTION DSN, not the
// looser one openTestDB uses. The pragmas are the measurement: _txlock=
// immediate is what makes every BeginTx take the write lock up front, and
// busy_timeout is what a blocked worker waits out. Benchmarking without them
// would measure a database this program never opens.
func openBenchDB(b *testing.B) *sql.DB {
	b.Helper()
	path := filepath.Join(b.TempDir(), "bench.db")
	hdb, err := history.Open(context.Background(), path)
	if err != nil {
		b.Fatalf("history.Open: %v", err)
	}
	if err := hdb.Close(); err != nil {
		b.Fatalf("close migration handle: %v", err)
	}
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		b.Fatalf("open bench db: %v", err)
	}
	b.Cleanup(func() {
		if err := db.Close(); err != nil {
			b.Errorf("close bench db: %v", err)
		}
	})
	return db
}

// BenchmarkFactLogAppend_SingleFact measures the hot path: one decoded
// article, one Append call, as pipeline.appendArticleFacts issues it.
func BenchmarkFactLogAppend_SingleFact(b *testing.B) {
	ctx := context.Background()
	fl := NewSQLiteFactLog(openBenchDB(b))
	for i := 0; b.Loop(); i++ {
		f := ArticleFact{FileIdx: 0, ArtIdx: int32(i), Offset: int64(i) * 100, Length: 100, CRC32: 1} //nolint:gosec // G115: bench counter
		if err := fl.Append(ctx, "job-1", []ArticleFact{f}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFactLogAppend_Contended is the case the per-article transaction
// actually costs: several pipeline workers appending at once against a
// database that permits one writer at a time.
func BenchmarkFactLogAppend_Contended(b *testing.B) {
	ctx := context.Background()
	fl := NewSQLiteFactLog(openBenchDB(b))
	const workers = 8
	var n int32
	var mu sync.Mutex
	next := func() int32 { mu.Lock(); defer mu.Unlock(); n++; return n }

	b.SetParallelism(workers)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := next()
			f := ArticleFact{FileIdx: 0, ArtIdx: i, Offset: int64(i) * 100, Length: 100, CRC32: 1}
			if err := fl.Append(ctx, "job-1", []ArticleFact{f}); err != nil {
				b.Fatal(err)
			}
		}
	})
	_ = fmt.Sprint(n)
}
