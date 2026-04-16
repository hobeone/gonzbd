package cache

import (
	"fmt"
	"testing"
)

const articleSize = 64 * 1024 // 64 KB — typical Usenet article

// poolSize is the number of distinct keys cycled through by the Hot and
// Parallel benchmarks. Chosen so total in-memory footprint is well under
// the 512 MB limit (poolSize × articleSize = 512 KB), regardless of b.N.
const poolSize = 8

// makePayload returns a deterministic byte slice of the given length.
func makePayload(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i % 256)
	}
	return b
}

// makeKeyPool returns a slice of poolSize distinct Message-ID keys.
func makeKeyPool(prefix string, n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s-%04d@bench.test", prefix, i)
	}
	return keys
}

// BenchmarkCacheSave_Hot measures Save throughput into a cache with plenty
// of room — every write stays in memory, no disk spill occurs.
// Keys cycle through a small pool so in-memory size stays bounded regardless
// of b.N.
func BenchmarkCacheSave_Hot(b *testing.B) {
	// Limit comfortably holds poolSize × articleSize with room to spare.
	c := New(Options{Limit: 512 * 1024 * 1024})
	adminDir := b.TempDir()
	payload := makePayload(articleSize)
	keys := makeKeyPool("msg-hot", poolSize)

	b.SetBytes(int64(articleSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := keys[i%poolSize]
		if err := c.Save(key, adminDir, payload); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCacheSaveLoad_RoundTrip measures the steady-state memory path:
// Save followed immediately by Load for the same key.
// Load is destructive so the key is immediately re-available for the next
// iteration; no accumulation occurs.
func BenchmarkCacheSaveLoad_RoundTrip(b *testing.B) {
	c := New(Options{Limit: 512 * 1024 * 1024})
	adminDir := b.TempDir()
	payload := makePayload(articleSize)
	const rtKey = "msg-roundtrip@bench.test"

	b.SetBytes(int64(articleSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Save(rtKey, adminDir, payload); err != nil {
			b.Fatal(err)
		}
		data, err := c.Load(rtKey, adminDir)
		if err != nil {
			b.Fatal(err)
		}
		if len(data) != articleSize {
			b.Fatalf("unexpected payload length: %d", len(data))
		}
	}
}

// BenchmarkCacheSave_Parallel measures mutex contention under concurrent
// savers — the realistic production scenario where many downloader goroutines
// submit articles simultaneously.
// Each goroutine cycles through its own small key pool to avoid unbounded
// memory growth across a long benchmark run.
func BenchmarkCacheSave_Parallel(b *testing.B) {
	c := New(Options{Limit: 512 * 1024 * 1024})
	adminDir := b.TempDir()
	payload := makePayload(articleSize)

	b.SetBytes(int64(articleSize))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		// Use the goroutine pointer as a namespace so goroutines don't
		// collide with each other's keys (which would merge writes in-memory
		// — still valid but changes the contention pattern).
		keys := makeKeyPool(fmt.Sprintf("msg-%p", pb), poolSize)
		i := 0
		for pb.Next() {
			if err := c.Save(keys[i%poolSize], adminDir, payload); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

// BenchmarkCacheSave_DiskSpill measures the disk-write path. The cache limit
// is set to less than one article so every Save spills to disk.
// Keys cycle through a fixed pool; the disk file for a key is overwritten on
// each reuse (existing behaviour of writeToDisk).
func BenchmarkCacheSave_DiskSpill(b *testing.B) {
	// Limit = 0 → no-cache mode: every Save goes straight to disk.
	c := New(Options{Limit: 0})
	adminDir := b.TempDir()
	payload := makePayload(articleSize)
	keys := makeKeyPool("msg-spill", poolSize)

	b.SetBytes(int64(articleSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := keys[i%poolSize]
		if err := c.Save(key, adminDir, payload); err != nil {
			b.Fatal(err)
		}
	}
}
