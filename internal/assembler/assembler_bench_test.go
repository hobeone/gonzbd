package assembler

import (
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkDiskThroughput measures the throughput with realistic article sizes.
func BenchmarkDiskThroughput(b *testing.B) {
	dir := b.TempDir()
	const partsPerFile = 100
	const articleSize = 700 * 1024 // 700KB

	opts := Options{
		FileInfo: func(jobID string, fileIdx int) (FileInfo, error) {
			return FileInfo{
				Path:       filepath.Join(dir, fmt.Sprintf("%s_%d.dat", jobID, fileIdx)),
				TotalParts: partsPerFile,
			}, nil
		},
	}

	a := New(opts, nil)
	if err := a.Start(b.Context()); err != nil {
		b.Fatalf("Start: %v", err)
	}
	payload := make([]byte, articleSize)

	b.ResetTimer()
	for i := range b.N {
		jobID := fmt.Sprintf("j%d", i)
		for p := range partsPerFile {
			req := WriteRequest{
				JobID:     jobID,
				FileIdx:   0,
				ArtIdx:    testArtIdx(p),
				MessageID: fmt.Sprintf("%s-%d", jobID, p),
				Offset:    int64(p * articleSize),
				Data:      payload, // Shared payload is fine for throughput test
			}
			if err := writeArticle(b.Context(), a, req); err != nil {
				b.Fatalf("WriteArticle: %v", err)
			}
		}
	}
	if err := a.Stop(); err != nil {
		b.Fatalf("Stop: %v", err)
	}
	b.StopTimer()
	b.SetBytes(int64(b.N * partsPerFile * articleSize))
}
