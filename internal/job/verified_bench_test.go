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
			arts := make([]JobArticle, n)
			for i := range arts {
				arts[i] = JobArticle{ID: fmt.Sprintf("<%d@x>", i), Bytes: 100}
			}
			m := newManifest([]JobFile{{Subject: "big.bin", Bytes: int64(n) * 100, Articles: arts}})
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
