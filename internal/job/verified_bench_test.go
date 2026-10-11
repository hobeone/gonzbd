package job

import (
	"fmt"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// BenchmarkMarkArticleWritten_WholeFile writes every article of a one-file
// job once, in order, the way the recorder does as a file downloads. One
// iteration is the whole file, so the time per op shows how writing a file
// scales with its article count.
func BenchmarkMarkArticleWritten_WholeFile(b *testing.B) {
	for _, n := range []int{1000, 5000, 20000} {
		b.Run(fmt.Sprintf("articles=%d", n), func(b *testing.B) {
			m := benchManifest(n)
			for b.Loop() {
				b.StopTimer()
				j := New("bench", "bench", Policy{})
				if err := j.AttachContent(m); err != nil {
					b.Fatalf("AttachContent: %v", err)
				}
				b.StartTimer()
				for i := range n {
					row := durability.WrittenRow{FileIdx: 0, ArtIdx: int32(i), Offset: int64(i) * 100, Length: 100} //nolint:gosec // G115: bench index
					if err := j.MarkArticleWritten(row); err != nil {
						b.Fatalf("MarkArticleWritten: %v", err)
					}
				}
			}
		})
	}
}

// benchManifest is one file of n 100-byte articles.
func benchManifest(n int) *Manifest {
	arts := make([]JobArticle, n)
	for i := range arts {
		arts[i] = JobArticle{ID: fmt.Sprintf("<%d@x>", i), Bytes: 100}
	}
	return newManifest([]JobFile{{Subject: "big.bin", Bytes: int64(n) * 100, Articles: arts}})
}

// halfFailedJob is a one-file job whose even articles are written, each with
// a resident row, and whose odd articles are failed.
func halfFailedJob(b *testing.B, m *Manifest) *Job {
	b.Helper()
	j := New("bench", "bench", Policy{})
	if err := j.AttachContent(m); err != nil {
		b.Fatalf("AttachContent: %v", err)
	}
	for i := range m.NumArticles() {
		if i%2 == 1 {
			if err := j.MarkArticleFailed(i); err != nil {
				b.Fatalf("MarkArticleFailed: %v", err)
			}
			continue
		}
		row := durability.WrittenRow{FileIdx: 0, ArtIdx: int32(i), Offset: int64(i) * 100, Length: 100} //nolint:gosec // G115: bench index
		if err := j.MarkArticleWritten(row); err != nil {
			b.Fatalf("MarkArticleWritten: %v", err)
		}
	}
	return j
}

// benchHalfFailedReset times one bulk reset of a halfFailedJob, which returns
// every failed article to Outstanding while the written ones keep their rows.
func benchHalfFailedReset(b *testing.B, reset func(*Job)) {
	for _, n := range []int{5000, 20000} {
		b.Run(fmt.Sprintf("articles=%d", n), func(b *testing.B) {
			m := benchManifest(n)
			for b.Loop() {
				b.StopTimer()
				j := halfFailedJob(b, m)
				b.StartTimer()
				reset(j)
			}
		})
	}
}

func BenchmarkClearEmittedForReload_HalfFailed(b *testing.B) {
	benchHalfFailedReset(b, func(j *Job) { j.ClearEmittedForReload(false) })
}

func BenchmarkResetForRetry_HalfFailed(b *testing.B) {
	benchHalfFailedReset(b, func(j *Job) { j.ResetForRetry() })
}
